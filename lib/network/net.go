package network

import (
	"bytes"
	"encoding"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	dbg "github.com/dedis/cothority/lib/debug_lvl"
	"github.com/dedis/crypto/abstract"
	"net"
	"os"
	"reflect"
	"time"
)

/// Encoding part ///

type Type uint8

var currType Type
var Suite abstract.Suite
var TypeRegistry = make(map[Type]reflect.Type)
var InvTypeRegistry = make(map[reflect.Type]Type)

// RegisterProtocolType register a custom "struct" / "packet" and get
// the allocated Type
// Pass simply an your non-initialized struct
func RegisterProtocolType(msg ProtocolMessage) Type {
	currType += 1
	t := reflect.TypeOf(msg)
	TypeRegistry[currType] = t
	InvTypeRegistry[t] = currType
	return currType
}

// String returns the underlying type in human format
func (t Type) String() string {
	ty, ok := TypeRegistry[t]
	if !ok {
		return "unknown"
	}
	return ty.Name()
}

// ProtocolMessage is a type for any message that the user wants to send
type ProtocolMessage interface{}

// ApplicationMessage is the container for any ProtocolMessage
type ApplicationMessage struct {
	MsgType Type
	Msg     ProtocolMessage
}

// MarshalBinary the application message => to bytes
// Implements BinaryMarshaler interface so it will be used when sending with gob
func (am *ApplicationMessage) MarshalBinary() ([]byte, error) {
	var buf = new(bytes.Buffer)
	err := binary.Write(buf, binary.BigEndian, am.MsgType)
	if err != nil {
		return nil, err
	}
	// if underlying type implements BinaryMarshal => use that
	if bm, ok := am.Msg.(encoding.BinaryMarshaler); ok {
		bufMsg, err := bm.MarshalBinary()
		if err != nil {
			return nil, err
		}
		_, err = buf.Write(bufMsg)
		if err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	// Otherwise, use Encoding from the Suite
	err = Suite.Write(buf, am.Msg)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// UnmarshalBinary will decode the incoming bytes
// It checks if the underlying packet is self-decodable
// by using its UnmarshalBinary interface
// otherwise, use abstract.Encoding (suite) to decode
func (am *ApplicationMessage) UnmarshalBinary(buf []byte) error {
	b := bytes.NewBuffer(buf)
	var t Type
	err := binary.Read(b, binary.BigEndian, &t)
	if err != nil {
		fmt.Printf("Error reading Type : %v\n", err)
		os.Exit(1)
	}

	ty, ok := TypeRegistry[t]
	if !ok {
		fmt.Printf("Type %d is not registered so we can not allocate this type %s\n", t, t.String())
		os.Exit(1)
	}

	am.MsgType = t

	// Look if the type supports UnmarshalBinary
	ptr := reflect.New(ty)
	v := ptr.Elem()
	if bu, ok := ptr.Interface().(encoding.BinaryUnmarshaler); ok {
		// Bytes() returns the UNREAD portion of bytes ;)
		err := bu.UnmarshalBinary(b.Bytes())
		am.Msg = ptr.Elem().Interface()
		return err
	}
	// Otherwise decode it ourself
	err = Suite.Read(b, ptr.Interface()) // v.Addr().Interface())
	if err != nil {
		fmt.Printf("Error decoding ProtocolMessage : %v\n", err)
		os.Exit(1)
	}
	am.Msg = v.Interface()
	//fmt.Printf("UnmarshalBinary() : Decoded type %s => %v\n", t.String(), ty)
	return nil
}

// ConstructFrom takes a ProtocolMessage and then construct a
// ApplicationMessage from it. Error if the type is unknown
func (am *ApplicationMessage) ConstructFrom(obj ProtocolMessage) error {
	t := reflect.TypeOf(obj)
	ty, ok := InvTypeRegistry[t]
	if !ok {
		return errors.New(fmt.Sprintf("Packet to send is not known. Please register packet : %s\n", t.String()))
	}
	am.MsgType = ty
	am.Msg = obj
	return nil
}

// Network part //

// How many times should we try to connect
const maxRetry = 5
const waitRetry = 1 * time.Second

// Host is the basic interface to represent a Host of any kind
// Host can open new Conn(ections) and Listen for any incoming Conn(...)
type Host interface {
	Name() string
	Open(name string) Conn
	Listen(addr string, fn func(Conn)) // the srv processing function
}

// Conn is the basic interface to represent any communication mean
// between two host. It is closely related to the underlying type of Host
// since a TcpHost will generate only TcpConn
type Conn interface {
	PeerName() string
	Send(obj ProtocolMessage) error
	Receive() (ApplicationMessage, error)
	Close()
}

// TcpHost is the underlying implementation of
// Host using Tcp as a communication channel
type TcpHost struct {
	// its name (usually its IP address)
	name string
	// A list of connection maintained by this host
	peers map[string]Conn
}

// TcpConn is the underlying implementation of
// Conn using Tcp
type TcpConn struct {
	// Peer is the name of the endpoint
	Peer string

	// The connection used
	Conn net.Conn
	// TcpConn uses Gob to encode / decode its messages
	enc *gob.Encoder
	dec *gob.Decoder
	// A pointer to the associated host (just-in-case)
	host *TcpHost
}

// PeerName returns the name of the peer at the end point of
// the conn
func (c *TcpConn) PeerName() string {
	return c.Peer
}

// Receive waits for any input on the connection and returns
// the ApplicationMessage **decoded** and an error if something
// wrong occured
func (c *TcpConn) Receive() (ApplicationMessage, error) {
	var am ApplicationMessage
	err := c.dec.Decode(&am)
	if err != nil {
		fmt.Printf("Error decoding ApplicationMessage : %v\n", err)
		os.Exit(1)
	}
	return am, nil
}

// Send will convert the Protocolmessage into an ApplicationMessage
// Then send the message through the Gob encoder
// Returns an error if anything was wrong
func (c *TcpConn) Send(obj ProtocolMessage) error {
	am := ApplicationMessage{}
	err := am.ConstructFrom(obj)
	if err != nil {
		fmt.Printf("Error converting packet : %v\n", err)
		os.Exit(1) // should not happen . I know.
	}
	err = c.enc.Encode(&am)
	if err != nil {
		fmt.Printf("Error sending application message ..")
		os.Exit(1)
	}
	return err
}

// Close ... closes the connection
func (c *TcpConn) Close() {
	err := c.Conn.Close()
	if err != nil {
		fmt.Printf("Error while closing tcp conn : %v\n", err)
		os.Exit(1)
	}
}

// NewTcpHost returns a Fresh TCP Host
func NewTcpHost(name string) *TcpHost {
	return &TcpHost{
		name:  name,
		peers: make(map[string]Conn),
	}
}

// Name is the name ofthis host
func (t *TcpHost) Name() string {
	return t.name
}

// Open will create a new connection between this host
// and the remote host named "name". This is a TcpConn.
// If anything went wrong, Conn will be nil.
func (t *TcpHost) Open(name string) Conn {
	var conn net.Conn
	var err error
	for i := 0; i < maxRetry; i++ {

		conn, err = net.Dial("tcp", name)
		if err != nil {
			fmt.Printf("%s (%d/%d) Error opening connection to %s\n", t.Name(), i, maxRetry, name)
		} else {
			break
		}
		time.Sleep(waitRetry)
	}
	if conn == nil {
		os.Exit(1)
	}
	c := TcpConn{
		Peer: name,
		Conn: conn,
		enc:  gob.NewEncoder(conn),
		dec:  gob.NewDecoder(conn),
		host: t,
	}
	t.peers[name] = &c
	return &c
}

// Listen for any host trying to contact him.
// Will launch in a goroutine the srv function once a connection is established
func (t *TcpHost) Listen(addr string, fn func(Conn)) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Printf("error listening (host %s)\n", t.name)
	}
	dbg.Lvl3(t.Name(), "Waiting for connections on addr ", addr, "..\n")
	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Printf("Error accepting connection : %v\n", err)
			os.Exit(1)
		}
		c := TcpConn{
			Peer: conn.RemoteAddr().String(),
			Conn: conn,
			enc:  gob.NewEncoder(conn),
			dec:  gob.NewDecoder(conn),
			host: t,
		}
		t.peers[conn.RemoteAddr().String()] = &c
		go fn(&c)
	}
}