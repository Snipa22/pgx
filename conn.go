// Package pgx is a PostgreSQL database driver.
//
// It does not implement the standard database/sql interface.
package pgx

import (
	"bufio"
	"crypto/md5"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	log "gopkg.in/inconshreveable/log15.v2"
	"io"
	"net"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ConnConfig contains all the options used to establish a connection.
type ConnConfig struct {
	Host      string // host (e.g. localhost) or path to unix domain socket directory (e.g. /private/tmp)
	Port      uint16 // default: 5432
	Database  string
	User      string // default: OS user name
	Password  string
	TLSConfig *tls.Config // config for TLS connection -- nil disables TLS
	Logger    log.Logger
}

// Conn is a PostgreSQL connection handle. It is not safe for concurrent usage.
// Use ConnPool to manage access to multiple database connections from multiple
// goroutines.
type Conn struct {
	conn               net.Conn      // the underlying TCP or unix domain socket connection
	reader             *bufio.Reader // buffered reader to improve read performance
	wbuf               [1024]byte
	Pid                int32             // backend pid
	SecretKey          int32             // key to use to send a cancel query message to the server
	RuntimeParams      map[string]string // parameters that have been reported by the server
	config             ConnConfig        // config used when establishing this connection
	TxStatus           byte
	preparedStatements map[string]*PreparedStatement
	notifications      []*Notification
	alive              bool
	causeOfDeath       error
	logger             log.Logger
	rows               Rows
	mr                 MsgReader
}

type PreparedStatement struct {
	Name              string
	FieldDescriptions []FieldDescription
	ParameterOids     []Oid
}

type Notification struct {
	Pid     int32  // backend pid that sent the notification
	Channel string // channel from which notification was received
	Payload string
}

type CommandTag string

// RowsAffected returns the number of rows affected. If the CommandTag was not
// for a row affecting command (such as "CREATE TABLE") then it returns 0
func (ct CommandTag) RowsAffected() int64 {
	words := strings.Split(string(ct), " ")
	n, _ := strconv.ParseInt(words[len(words)-1], 10, 64)
	return n
}

var ErrNoRows = errors.New("no rows in result set")
var ErrNotificationTimeout = errors.New("notification timeout")
var ErrDeadConn = errors.New("conn is dead")

type ProtocolError string

func (e ProtocolError) Error() string {
	return string(e)
}

// Connect establishes a connection with a PostgreSQL server using config.
// config.Host must be specified. config.User will default to the OS user name.
// Other config fields are optional.
func Connect(config ConnConfig) (c *Conn, err error) {
	c = new(Conn)

	c.config = config
	if c.config.Logger != nil {
		c.logger = c.config.Logger
	} else {
		c.logger = log.New()
		c.logger.SetHandler(log.DiscardHandler())
	}

	if c.config.User == "" {
		user, err := user.Current()
		if err != nil {
			return nil, err
		}
		c.config.User = user.Username
		c.logger.Debug("Using default connection config", "User", c.config.User)
	}

	if c.config.Port == 0 {
		c.config.Port = 5432
		c.logger.Debug("Using default connection config", "Port", c.config.Port)
	}

	// See if host is a valid path, if yes connect with a socket
	_, err = os.Stat(c.config.Host)
	if err == nil {
		// For backward compatibility accept socket file paths -- but directories are now preferred
		socket := c.config.Host
		if !strings.Contains(socket, "/.s.PGSQL.") {
			socket = filepath.Join(socket, ".s.PGSQL.") + strconv.FormatInt(int64(c.config.Port), 10)
		}

		c.logger.Info(fmt.Sprintf("Dialing PostgreSQL server at socket: %s", socket))
		c.conn, err = net.Dial("unix", socket)
		if err != nil {
			c.logger.Error(fmt.Sprintf("Connection failed: %v", err))
			return nil, err
		}
	} else {
		c.logger.Info(fmt.Sprintf("Dialing PostgreSQL server at host: %s:%d", c.config.Host, c.config.Port))
		c.conn, err = net.Dial("tcp", fmt.Sprintf("%s:%d", c.config.Host, c.config.Port))
		if err != nil {
			c.logger.Error(fmt.Sprintf("Connection failed: %v", err))
			return nil, err
		}
	}
	defer func() {
		if c != nil && err != nil {
			c.conn.Close()
			c.alive = false
			c.logger.Error(err.Error())
		}
	}()

	c.RuntimeParams = make(map[string]string)
	c.preparedStatements = make(map[string]*PreparedStatement)
	c.alive = true

	if config.TLSConfig != nil {
		c.logger.Debug("Starting TLS handshake")
		if err = c.startTLS(); err != nil {
			c.logger.Error(fmt.Sprintf("TLS failed: %v", err))
			return
		}
	}

	c.reader = bufio.NewReader(c.conn)
	c.mr.reader = c.reader

	msg := newStartupMessage()
	msg.options["user"] = c.config.User
	if c.config.Database != "" {
		msg.options["database"] = c.config.Database
	}
	if err = c.txStartupMessage(msg); err != nil {
		return
	}

	for {
		var t byte
		var r *MsgReader
		t, r, err = c.rxMsg()
		if err != nil {
			return nil, err
		}

		switch t {
		case backendKeyData:
			c.rxBackendKeyData(r)
		case authenticationX:
			if err = c.rxAuthenticationX(r); err != nil {
				return nil, err
			}
		case readyForQuery:
			c.rxReadyForQuery(r)
			c.logger = c.logger.New("pid", c.Pid)
			c.logger.Info("Connection established")
			return c, nil
		default:
			if err = c.processContextFreeMsg(t, r); err != nil {
				return nil, err
			}
		}
	}
}

// Close closes a connection. It is safe to call Close on a already closed
// connection.
func (c *Conn) Close() (err error) {
	if !c.IsAlive() {
		return nil
	}

	wbuf := newWriteBuf(c.wbuf[0:0], 'X')
	wbuf.closeMsg()

	_, err = c.conn.Write(wbuf.buf)

	c.die(errors.New("Closed"))
	c.logger.Info("Closed connection")
	return err
}

// ParseURI parses a database URI into ConnConfig
func ParseURI(uri string) (ConnConfig, error) {
	var cp ConnConfig

	url, err := url.Parse(uri)
	if err != nil {
		return cp, err
	}

	if url.User != nil {
		cp.User = url.User.Username()
		cp.Password, _ = url.User.Password()
	}

	parts := strings.SplitN(url.Host, ":", 2)
	cp.Host = parts[0]
	if len(parts) == 2 {
		p, err := strconv.ParseUint(parts[1], 10, 16)
		if err != nil {
			return cp, err
		}
		cp.Port = uint16(p)
	}
	cp.Database = strings.TrimLeft(url.Path, "/")

	return cp, nil
}

// Prepare creates a prepared statement with name and sql. sql can contain placeholders
// for bound parameters. These placeholders are referenced positional as $1, $2, etc.
func (c *Conn) Prepare(name, sql string) (ps *PreparedStatement, err error) {
	defer func() {
		if err != nil {
			c.logger.Error(fmt.Sprintf("Prepare `%s` as `%s` failed: %v", name, sql, err))
		}
	}()

	// parse
	wbuf := newWriteBuf(c.wbuf[0:0], 'P')
	wbuf.WriteCString(name)
	wbuf.WriteCString(sql)
	wbuf.WriteInt16(0)

	// describe
	wbuf.startMsg('D')
	wbuf.WriteByte('S')
	wbuf.WriteCString(name)

	// sync
	wbuf.startMsg('S')
	wbuf.closeMsg()

	_, err = c.conn.Write(wbuf.buf)
	if err != nil {
		return nil, err
	}

	ps = &PreparedStatement{Name: name}

	var softErr error

	for {
		var t byte
		var r *MsgReader
		t, r, err := c.rxMsg()
		if err != nil {
			return nil, err
		}

		switch t {
		case parseComplete:
		case parameterDescription:
			ps.ParameterOids = c.rxParameterDescription(r)
		case rowDescription:
			ps.FieldDescriptions = c.rxRowDescription(r)
			for i := range ps.FieldDescriptions {
				switch ps.FieldDescriptions[i].DataType {
				case BoolOid, ByteaOid, Int2Oid, Int4Oid, Int8Oid, Float4Oid, Float8Oid, DateOid, TimestampTzOid:
					ps.FieldDescriptions[i].FormatCode = BinaryFormatCode
				}
			}
		case noData:
		case readyForQuery:
			c.rxReadyForQuery(r)
			c.preparedStatements[name] = ps
			return ps, softErr
		default:
			if e := c.processContextFreeMsg(t, r); e != nil && softErr == nil {
				softErr = e
			}
		}
	}
}

// Deallocate released a prepared statement
func (c *Conn) Deallocate(name string) (err error) {
	delete(c.preparedStatements, name)
	_, err = c.Exec("deallocate " + QuoteIdentifier(name))
	return
}

// Listen establishes a PostgreSQL listen/notify to channel
func (c *Conn) Listen(channel string) (err error) {
	_, err = c.Exec("listen " + channel)
	return
}

// WaitForNotification waits for a PostgreSQL notification for up to timeout.
// If the timeout occurs it returns pgx.ErrNotificationTimeout
func (c *Conn) WaitForNotification(timeout time.Duration) (*Notification, error) {
	if len(c.notifications) > 0 {
		notification := c.notifications[0]
		c.notifications = c.notifications[1:]
		return notification, nil
	}

	var zeroTime time.Time
	stopTime := time.Now().Add(timeout)

	for {
		// Use SetReadDeadline to implement the timeout. SetReadDeadline will
		// cause operations to fail with a *net.OpError that has a Timeout()
		// of true. Because the normal pgx rxMsg path considers any error to
		// have potentially corrupted the state of the connection, it dies
		// on any errors. So to avoid timeout errors in rxMsg we set the
		// deadline and peek into the reader. If a timeout error occurs there
		// we don't break the pgx connection. If the Peek returns that data
		// is available then we turn off the read deadline before the rxMsg.
		err := c.conn.SetReadDeadline(stopTime)
		if err != nil {
			return nil, err
		}

		// Wait until there is a byte available before continuing onto the normal msg reading path
		_, err = c.reader.Peek(1)
		if err != nil {
			c.conn.SetReadDeadline(zeroTime) // we can only return one error and we already have one -- so ignore possiple error from SetReadDeadline
			if err, ok := err.(*net.OpError); ok && err.Timeout() {
				return nil, ErrNotificationTimeout
			}
			return nil, err
		}

		err = c.conn.SetReadDeadline(zeroTime)
		if err != nil {
			return nil, err
		}

		var t byte
		var r *MsgReader
		if t, r, err = c.rxMsg(); err == nil {
			if err = c.processContextFreeMsg(t, r); err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}

		if len(c.notifications) > 0 {
			notification := c.notifications[0]
			c.notifications = c.notifications[1:]
			return notification, nil
		}
	}
}

func (c *Conn) IsAlive() bool {
	return c.alive
}

func (c *Conn) CauseOfDeath() error {
	return c.causeOfDeath
}

type Row Rows

func (r *Row) Scan(dest ...interface{}) (err error) {
	rows := (*Rows)(r)

	if rows.Err() != nil {
		return rows.Err()
	}

	if !rows.Next() {
		if rows.Err() == nil {
			return ErrNoRows
		} else {
			return rows.Err()
		}
	}

	rows.Scan(dest...)
	rows.Close()
	return rows.Err()
}

type Rows struct {
	pool      *ConnPool
	conn      *Conn
	mr        *MsgReader
	fields    []FieldDescription
	rowCount  int
	columnIdx int
	err       error
	closed    bool
}

func (rows *Rows) FieldDescriptions() []FieldDescription {
	return rows.fields
}

func (rows *Rows) MsgReader() *MsgReader {
	return rows.mr
}

func (rows *Rows) close() {
	if rows.pool != nil {
		rows.pool.Release(rows.conn)
		rows.pool = nil
	}

	rows.closed = true
}

func (rows *Rows) readUntilReadyForQuery() {
	for {
		t, r, err := rows.conn.rxMsg()
		if err != nil {
			rows.close()
			return
		}

		switch t {
		case readyForQuery:
			rows.conn.rxReadyForQuery(r)
			rows.close()
			return
		case rowDescription:
		case dataRow:
		case commandComplete:
		case bindComplete:
		default:
			err = rows.conn.processContextFreeMsg(t, r)
			if err != nil {
				rows.close()
				return
			}
		}
	}
}

func (rows *Rows) Close() {
	if rows.closed {
		return
	}
	rows.readUntilReadyForQuery()
	rows.close()
}

func (rows *Rows) Err() error {
	return rows.err
}

// abort signals that the query was not successfully sent to the server.
// This differs from Fatal in that it is not necessary to readUntilReadyForQuery
func (rows *Rows) abort(err error) {
	if rows.err != nil {
		return
	}

	rows.err = err
	rows.close()
}

// Fatal signals an error occurred after the query was sent to the server
func (rows *Rows) Fatal(err error) {
	if rows.err != nil {
		return
	}

	rows.err = err
	rows.Close()
}

func (rows *Rows) Next() bool {
	if rows.closed {
		return false
	}

	rows.rowCount++
	rows.columnIdx = 0

	for {
		t, r, err := rows.conn.rxMsg()
		if err != nil {
			rows.Fatal(err)
			return false
		}

		switch t {
		case readyForQuery:
			rows.conn.rxReadyForQuery(r)
			rows.close()
			return false
		case dataRow:
			fieldCount := r.ReadInt16()
			if int(fieldCount) != len(rows.fields) {
				rows.Fatal(ProtocolError(fmt.Sprintf("Row description field count (%v) and data row field count (%v) do not match", len(rows.fields), fieldCount)))
				return false
			}

			rows.mr = r
			return true
		case commandComplete:
		case bindComplete:
		default:
			err = rows.conn.processContextFreeMsg(t, r)
			if err != nil {
				rows.Fatal(err)
				return false
			}
		}
	}
}

func (rows *Rows) nextColumn() (*FieldDescription, int32, bool) {
	if rows.closed {
		return nil, 0, false
	}
	if len(rows.fields) <= rows.columnIdx {
		rows.Fatal(ProtocolError("No next column available"))
		return nil, 0, false
	}

	fd := &rows.fields[rows.columnIdx]
	rows.columnIdx++
	size := rows.mr.ReadInt32()
	return fd, size, true
}

func (rows *Rows) Scan(dest ...interface{}) (err error) {
	if len(rows.fields) != len(dest) {
		err = errors.New("Scan received wrong number of arguments")
		rows.Fatal(err)
		return err
	}

	for _, d := range dest {
		fd, size, _ := rows.nextColumn()
		switch d := d.(type) {
		case *bool:
			*d = decodeBool(rows, fd, size)
		case *[]byte:
			*d = decodeBytea(rows, fd, size)
		case *int64:
			*d = decodeInt8(rows, fd, size)
		case *int16:
			*d = decodeInt2(rows, fd, size)
		case *int32:
			*d = decodeInt4(rows, fd, size)
		case *string:
			*d = decodeText(rows, fd, size)
		case *float32:
			*d = decodeFloat4(rows, fd, size)
		case *float64:
			*d = decodeFloat8(rows, fd, size)
		case *time.Time:
			if fd.DataType == DateOid {
				*d = decodeDate(rows, fd, size)
			} else {
				*d = decodeTimestampTz(rows, fd, size)
			}

		case Scanner:
			err = d.Scan(rows, fd, size)
			if err != nil {
				return err
			}
		default:
			return errors.New("Unknown type")
		}
	}

	return nil
}

// Values returns an array of the row values
func (rows *Rows) Values() ([]interface{}, error) {
	if rows.closed {
		return nil, errors.New("rows is closed")
	}

	values := make([]interface{}, 0, len(rows.fields))

	for _, _ = range rows.fields {
		if rows.Err() != nil {
			return nil, rows.Err()
		}

		fd, size, _ := rows.nextColumn()

		switch fd.DataType {
		case BoolOid:
			values = append(values, decodeBool(rows, fd, size))
		case ByteaOid:
			values = append(values, decodeBytea(rows, fd, size))
		case Int8Oid:
			values = append(values, decodeInt8(rows, fd, size))
		case Int2Oid:
			values = append(values, decodeInt2(rows, fd, size))
		case Int4Oid:
			values = append(values, decodeInt4(rows, fd, size))
		case VarcharOid, TextOid:
			values = append(values, decodeText(rows, fd, size))
		case Float4Oid:
			values = append(values, decodeFloat4(rows, fd, size))
		case Float8Oid:
			values = append(values, decodeFloat8(rows, fd, size))
		case DateOid:
			values = append(values, decodeDate(rows, fd, size))
		case TimestampTzOid:
			values = append(values, decodeTimestampTz(rows, fd, size))
		default:
			// if it is not an intrinsic type then return the text
			switch fd.FormatCode {
			case TextFormatCode:
				values = append(values, rows.MsgReader().ReadString(size))
			case BinaryFormatCode:
				return nil, errors.New("Values cannot handle binary format non-intrinsic types")
			default:
				return nil, errors.New("Unknown format code")
			}
		}
	}

	return values, rows.Err()
}

// TODO - document
func (c *Conn) Query(sql string, args ...interface{}) (*Rows, error) {
	c.rows = Rows{conn: c}
	rows := &c.rows

	if ps, present := c.preparedStatements[sql]; present {
		rows.fields = ps.FieldDescriptions
		err := c.sendPreparedQuery(ps, args...)
		if err != nil {
			rows.abort(err)
		}
		return rows, rows.err
	}

	err := c.sendSimpleQuery(sql, args...)
	if err != nil {
		rows.abort(err)
		return rows, rows.err
	}

	// Simple queries don't know the field descriptions of the result.
	// Read until that is known before returning
	for {
		t, r, err := c.rxMsg()
		if err != nil {
			rows.Fatal(err)
			return rows, rows.err
		}

		switch t {
		case rowDescription:
			rows.fields = rows.conn.rxRowDescription(r)
			return rows, nil
		default:
			err = rows.conn.processContextFreeMsg(t, r)
			if err != nil {
				rows.Fatal(err)
				return rows, rows.err
			}
		}
	}
}

func (c *Conn) QueryRow(sql string, args ...interface{}) *Row {
	rows, _ := c.Query(sql, args...)
	return (*Row)(rows)
}

func (c *Conn) sendQuery(sql string, arguments ...interface{}) (err error) {
	if ps, present := c.preparedStatements[sql]; present {
		return c.sendPreparedQuery(ps, arguments...)
	} else {
		return c.sendSimpleQuery(sql, arguments...)
	}
}

func (c *Conn) sendSimpleQuery(sql string, arguments ...interface{}) (err error) {
	if len(arguments) > 0 {
		sql, err = SanitizeSql(sql, arguments...)
		if err != nil {
			return
		}
	}

	wbuf := newWriteBuf(c.wbuf[0:0], 'Q')
	wbuf.WriteCString(sql)
	wbuf.closeMsg()

	_, err = c.conn.Write(wbuf.buf)

	return err
}

func (c *Conn) sendPreparedQuery(ps *PreparedStatement, arguments ...interface{}) (err error) {
	if len(ps.ParameterOids) != len(arguments) {
		return fmt.Errorf("Prepared statement \"%v\" requires %d parameters, but %d were provided", ps.Name, len(ps.ParameterOids), len(arguments))
	}

	// bind
	wbuf := newWriteBuf(c.wbuf[0:0], 'B')
	wbuf.WriteByte(0)
	wbuf.WriteCString(ps.Name)

	wbuf.WriteInt16(int16(len(ps.ParameterOids)))
	for i, oid := range ps.ParameterOids {
		switch arg := arguments[i].(type) {
		case BinaryEncoder:
			wbuf.WriteInt16(BinaryFormatCode)
		case TextEncoder:
			wbuf.WriteInt16(TextFormatCode)
		default:
			switch oid {
			case BoolOid, ByteaOid, Int2Oid, Int4Oid, Int8Oid, Float4Oid, Float8Oid, TimestampTzOid:
				wbuf.WriteInt16(BinaryFormatCode)
			case TextOid, VarcharOid, DateOid:
				wbuf.WriteInt16(TextFormatCode)
			default:
				return SerializationError(fmt.Sprintf("Parameter %d oid %d is not a core type and argument type %T does not implement TextEncoder or BinaryEncoder", i, oid, arg))
			}
		}
	}

	wbuf.WriteInt16(int16(len(arguments)))
	for i, oid := range ps.ParameterOids {
		if arguments[i] == nil {
			wbuf.WriteInt32(-1)
			continue
		}

		switch arg := arguments[i].(type) {
		case BinaryEncoder:
			err = arg.EncodeBinary(wbuf, &ps.FieldDescriptions[i])
		case TextEncoder:
			var s string
			var status byte
			s, status, err = arg.EncodeText()
			if status == NullText {
				wbuf.WriteInt32(-1)
				continue
			}
			wbuf.WriteInt32(int32(len(s)))
			wbuf.WriteBytes([]byte(s))
		default:
			switch oid {
			case BoolOid:
				err = encodeBool(wbuf, arguments[i])
			case ByteaOid:
				err = encodeBytea(wbuf, arguments[i])
			case Int2Oid:
				err = encodeInt2(wbuf, arguments[i])
			case Int4Oid:
				err = encodeInt4(wbuf, arguments[i])
			case Int8Oid:
				err = encodeInt8(wbuf, arguments[i])
			case Float4Oid:
				err = encodeFloat4(wbuf, arguments[i])
			case Float8Oid:
				err = encodeFloat8(wbuf, arguments[i])
			case TextOid, VarcharOid:
				err = encodeText(wbuf, arguments[i])
			case DateOid:
				err = encodeDate(wbuf, arguments[i])
			case TimestampTzOid:
				err = encodeTimestampTz(wbuf, arguments[i])
			default:
				return SerializationError(fmt.Sprintf("%T is not a core type and it does not implement TextEncoder or BinaryEncoder", arg))
			}
		}
		if err != nil {
			return err
		}
	}

	wbuf.WriteInt16(int16(len(ps.FieldDescriptions)))
	for _, fd := range ps.FieldDescriptions {
		wbuf.WriteInt16(fd.FormatCode)
	}

	// execute
	wbuf.startMsg('E')
	wbuf.WriteByte(0)
	wbuf.WriteInt32(0)

	// sync
	wbuf.startMsg('S')
	wbuf.closeMsg()

	_, err = c.conn.Write(wbuf.buf)

	return err
}

// Exec executes sql. sql can be either a prepared statement name or an SQL string.
// arguments will be sanitized before being interpolated into sql strings. arguments
// should be referenced positionally from the sql string as $1, $2, etc.
func (c *Conn) Exec(sql string, arguments ...interface{}) (commandTag CommandTag, err error) {
	startTime := time.Now()

	defer func() {
		if err == nil {
			endTime := time.Now()
			c.logger.Info("Exec", "sql", sql, "args", arguments, "time", endTime.Sub(startTime))
		} else {
			c.logger.Error("Exec", "sql", sql, "args", arguments, "error", err)
		}
	}()

	if err = c.sendQuery(sql, arguments...); err != nil {
		return
	}

	var softErr error

	for {
		var t byte
		var r *MsgReader
		t, r, err = c.rxMsg()
		if err != nil {
			return commandTag, err
		}

		switch t {
		case readyForQuery:
			c.rxReadyForQuery(r)
			return commandTag, softErr
		case rowDescription:
		case dataRow:
		case bindComplete:
		case commandComplete:
			commandTag = CommandTag(r.ReadCString())
		default:
			if e := c.processContextFreeMsg(t, r); e != nil && softErr == nil {
				softErr = e
			}
		}
	}
}

// Processes messages that are not exclusive to one context such as
// authentication or query response. The response to these messages
// is the same regardless of when they occur.
func (c *Conn) processContextFreeMsg(t byte, r *MsgReader) (err error) {
	switch t {
	case 'S':
		c.rxParameterStatus(r)
		return nil
	case errorResponse:
		return c.rxErrorResponse(r)
	case noticeResponse:
		return nil
	case notificationResponse:
		c.rxNotificationResponse(r)
		return nil
	default:
		return fmt.Errorf("Received unknown message type: %c", t)
	}
}

func (c *Conn) rxMsg() (t byte, r *MsgReader, err error) {
	if !c.alive {
		return 0, nil, ErrDeadConn
	}

	t, err = c.mr.rxMsg()
	if err != nil {
		c.die(err)
	}

	return t, &c.mr, err
}

func (c *Conn) rxAuthenticationX(r *MsgReader) (err error) {
	switch r.ReadInt32() {
	case 0: // AuthenticationOk
	case 3: // AuthenticationCleartextPassword
		err = c.txPasswordMessage(c.config.Password)
	case 5: // AuthenticationMD5Password
		salt := r.ReadString(4)
		digestedPassword := "md5" + hexMD5(hexMD5(c.config.Password+c.config.User)+salt)
		err = c.txPasswordMessage(digestedPassword)
	default:
		err = errors.New("Received unknown authentication message")
	}

	return
}

func hexMD5(s string) string {
	hash := md5.New()
	io.WriteString(hash, s)
	return hex.EncodeToString(hash.Sum(nil))
}

func (c *Conn) rxParameterStatus(r *MsgReader) {
	key := r.ReadCString()
	value := r.ReadCString()
	c.RuntimeParams[key] = value
}

func (c *Conn) rxErrorResponse(r *MsgReader) (err PgError) {
	for {
		switch r.ReadByte() {
		case 'S':
			err.Severity = r.ReadCString()
		case 'C':
			err.Code = r.ReadCString()
		case 'M':
			err.Message = r.ReadCString()
		case 0: // End of error message
			if err.Severity == "FATAL" {
				c.die(err)
			}
			return
		default: // Ignore other error fields
			r.ReadCString()
		}
	}
}

func (c *Conn) rxBackendKeyData(r *MsgReader) {
	c.Pid = r.ReadInt32()
	c.SecretKey = r.ReadInt32()
}

func (c *Conn) rxReadyForQuery(r *MsgReader) {
	c.TxStatus = r.ReadByte()
}

func (c *Conn) rxRowDescription(r *MsgReader) (fields []FieldDescription) {
	fieldCount := r.ReadInt16()
	fields = make([]FieldDescription, fieldCount)
	for i := int16(0); i < fieldCount; i++ {
		f := &fields[i]
		f.Name = r.ReadCString()
		f.Table = r.ReadOid()
		f.AttributeNumber = r.ReadInt16()
		f.DataType = r.ReadOid()
		f.DataTypeSize = r.ReadInt16()
		f.Modifier = r.ReadInt32()
		f.FormatCode = r.ReadInt16()
	}
	return
}

func (c *Conn) rxParameterDescription(r *MsgReader) (parameters []Oid) {
	parameterCount := r.ReadInt16()
	parameters = make([]Oid, 0, parameterCount)

	for i := int16(0); i < parameterCount; i++ {
		parameters = append(parameters, r.ReadOid())
	}
	return
}

func (c *Conn) rxNotificationResponse(r *MsgReader) {
	n := new(Notification)
	n.Pid = r.ReadInt32()
	n.Channel = r.ReadCString()
	n.Payload = r.ReadCString()
	c.notifications = append(c.notifications, n)
}

func (c *Conn) startTLS() (err error) {
	err = binary.Write(c.conn, binary.BigEndian, []int32{8, 80877103})
	if err != nil {
		return
	}

	response := make([]byte, 1)
	if _, err = io.ReadFull(c.conn, response); err != nil {
		return
	}

	if response[0] != 'S' {
		err = errors.New("Could not use TLS")
		return
	}

	c.conn = tls.Client(c.conn, c.config.TLSConfig)

	return nil
}

func (c *Conn) txStartupMessage(msg *startupMessage) error {
	_, err := c.conn.Write(msg.Bytes())
	return err
}

func (c *Conn) txPasswordMessage(password string) (err error) {
	wbuf := newWriteBuf(c.wbuf[0:0], 'p')
	wbuf.WriteCString(password)
	wbuf.closeMsg()

	_, err = c.conn.Write(wbuf.buf)

	return err
}

func (c *Conn) die(err error) {
	c.alive = false
	c.causeOfDeath = err
	c.conn.Close()
}
