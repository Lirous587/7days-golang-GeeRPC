package geerpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"geerpc/codec"
	"go/ast"
	"io"
	"log"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const MagicNumber = 0x3bef5c

type Option struct {
	MagicNumber    int           // MagicNumber marks this's a geerpc request
	CodecType      codec.Type    // client may choose different Codec to encode body
	ConnectTimeout time.Duration // 0 means no limit
	HandleTimeout  time.Duration
}

var DefaultOption = &Option{
	MagicNumber:    MagicNumber,
	CodecType:      codec.GobType,
	ConnectTimeout: time.Second * 10,
}

// 可能的报文格式 | Option | Header1 | Body1 | Header2 | Body2 | ...

// Server is an RPC Server.
type Server struct {
	serviceMap sync.Map
}

// Register publishes in the server the set of methods of the
func (server *Server) Register(rcvr interface{}) error {
	s := newService(rcvr)
	if _, dup := server.serviceMap.LoadOrStore(s.name, s); dup {
		return errors.New("rpc: service already defined: " + s.name)
	}
	return nil
}

func (server *Server) findService(serviceMethod string) (svc *service, mtype *methodType, err error) {
	dot := strings.LastIndex(serviceMethod, ".")
	if dot < 0 {
		err = errors.New("rpc server: service/method request ill-formed: " + serviceMethod)
		return
	}
	serviceName, methodName := serviceMethod[:dot], serviceMethod[dot+1:]
	svci, ok := server.serviceMap.Load(serviceName)
	if !ok {
		err = errors.New("rpc server: can't find service " + serviceName)
		return
	}
	svc = svci.(*service)
	mtype = svc.method[methodName]
	if mtype == nil {
		err = errors.New("rpc server: can't find method " + methodName)
	}
	return
}

// Register publishes the reveiver's methods in the DefaultServer.
func Register(rcvr interface{}) error { return DefaultServer.Register(rcvr) }

func NewServer() *Server {
	return &Server{}
}

// DefaultServer is the default instance of *Server.
var DefaultServer = NewServer()

// Accept accepts connections on the listener and serves requests
// for each incoming connection.
func (server *Server) Accept(lis net.Listener) {
	for {
		conn, err := lis.Accept()
		if err != nil {
			log.Println("rpc server: accept error: ", err)
			// Check if the error is caused by closing the listener.
			// If so, exit the Accept loop gracefully.
			if strings.Contains(err.Error(), "use of closed network connection") {
				return // exit directly
			}
		}
		go server.ServeConn(conn)
	}
}

// Accept accepts connections on the listener and serves requests
// for each incoming connection.
// This Accept handler is via DefaultServer
func Accept(lis net.Listener) { DefaultServer.Accept(lis) }

// ServeConn runs the server on a single connection.
// ServerCoon blocks,serving the connection until the client hangs up.
func (server *Server) ServeConn(conn io.ReadWriteCloser) {
	defer func() { _ = conn.Close() }()

	var opt Option
	// decode opt form conn
	if err := json.NewDecoder(conn).Decode(&opt); err != nil {
		log.Println("rpc server: options error: ", err)
		return
	}

	if opt.MagicNumber != MagicNumber {
		log.Printf("rpc server: invalid magic number %x", opt.MagicNumber)
		return
	}

	f := codec.NewCodecFuncMap[opt.CodecType]
	if f == nil {
		log.Printf("rpc server: invalid code type %s", opt.CodecType)
		return
	}

	// If all ok,will serve this conn
	server.serveCodec(f(conn), opt.HandleTimeout)
}

var invalidRequest = struct{}{}

// serveCodec is the main loop for processing requests on a single connection.
// Handle requests concurrently.
// Responses may be sent back out of order.The `sending` mutex ensures
// that the write for a single response(Header and Body) is atomic.
// This prevents the data from different responses from being interleaved on the wire.
// On other words, the data must satisfy the consistency between header and body: h2 body2 | h1 body1 | h4 body4 | h3 body3 (there numerical order does not matter)
// As for the client how to handle result? It need seq to ensure.
// How should th e client handle the results? It needs a seq to ensure.
// The loop continues until an unrecoverable error occurs or the client closes the connection.
// serveCodec handles three steps
// 1.Read the request
// 2.Process the request
// 3.Respond to the request
func (server *Server) serveCodec(cc codec.Codec, timeout time.Duration) {
	sending := new(sync.Mutex) // make sure to send a complete response
	wg := new(sync.WaitGroup)  // wait until all request handled
	for {
		req, err := server.readRequest(cc)
		if err != nil {
			if req == nil {
				break // it's not possible to recover,so close the connection. so cool
			}
			req.h.Error = err.Error()
			// response invalidRequest
			server.sendResponse(cc, req.h, invalidRequest, sending)
			continue
		}

		wg.Add(1)

		go server.handleRequest(cc, req, sending, wg, timeout)
	}
	wg.Wait()
	_ = cc.Close()
}

// request stores all information of a call
type request struct {
	h            *codec.Header // header of request
	argv, replyv reflect.Value // argv and replyv of request
	mtype        *methodType
	svc          *service
}

func (server *Server) readRequestHeader(cc codec.Codec) (*codec.Header, error) {
	var h codec.Header
	if err := cc.ReadHeader(&h); err != nil {
		if err != io.EOF && err != io.ErrUnexpectedEOF {
			log.Println("rpc  server: read header error: ", err)
		}
		return nil, err
	}
	return &h, nil
}

func (server *Server) readRequest(cc codec.Codec) (*request, error) {
	h, err := server.readRequestHeader(cc)
	if err != nil {
		return nil, err
	}
	req := &request{h: h}
	req.svc, req.mtype, err = server.findService(h.ServiceMethod)
	if err != nil {
		return req, err
	}
	req.argv = req.mtype.newArgv()
	req.replyv = req.mtype.newReplyv()

	// make sure that argvi is a pointer, Readbody need a pointer as parameter
	argvi := req.argv.Interface()
	if req.argv.Type().Kind() != reflect.Pointer {
		argvi = req.argv.Addr().Interface()
	}
	if err = cc.ReadBody(argvi); err != nil {
		log.Println("rpc server: read argv err: ", err)
		return req, err
	}
	return req, nil
}

func (server *Server) sendResponse(cc codec.Codec, h *codec.Header, body interface{}, sending *sync.Mutex) {
	sending.Lock()
	defer sending.Unlock()

	if err := cc.Write(h, body); err != nil {
		log.Panicln("rpc server: write response error: ", err)
	}
}

func (server *Server) handleRequest(cc codec.Codec, req *request, sending *sync.Mutex, wg *sync.WaitGroup, timeout time.Duration) {
	defer wg.Done()
	// use a buffered channel to receive completion signals to prevent goroutine leaks.
	finished := make(chan error, 1)
	go func() {
		err := req.svc.call(req.mtype, req.argv, req.replyv)
		finished <- err
	}()

	if timeout == 0 {
		err := <-finished
		if err != nil {
			req.h.Error = err.Error()
			server.sendResponse(cc, req.h, invalidRequest, sending)
			return
		}
		server.sendResponse(cc, req.h, req.replyv.Interface(), sending)
		return
	}

	select {
	case <-time.After(timeout): // timeout
		req.h.Error = fmt.Errorf("rpc server: request handle timeout: expect within %d", timeout).Error()
		server.sendResponse(cc, req.h, invalidRequest, sending)
	case err := <-finished: // no timeout
		if err != nil {
			req.h.Error = err.Error()
			server.sendResponse(cc, req.h, invalidRequest, sending)
			return
		}
		server.sendResponse(cc, req.h, req.replyv.Interface(), sending)
	}
}

type methodType struct {
	method    reflect.Method
	ArgType   reflect.Type
	ReplyType reflect.Type
	numCalls  uint64
}

func (m *methodType) NumCalls() uint64 {
	return atomic.LoadUint64(&m.numCalls)
}

func (m *methodType) newArgv() reflect.Value {
	var argv reflect.Value
	// arg may be a pointer type, or a value type
	if m.ArgType.Kind() == reflect.Pointer {
		argv = reflect.New(m.ArgType.Elem())
	} else {
		argv = reflect.New(m.ArgType).Elem()
	}
	return argv
}

func (m *methodType) newReplyv() reflect.Value {
	// reply must be a pointer type
	replyv := reflect.New(m.ReplyType.Elem())
	switch m.ReplyType.Elem().Kind() {
	case reflect.Map:
		replyv.Elem().Set(reflect.MakeMap(m.ReplyType.Elem()))
	case reflect.Slice:
		replyv.Elem().Set(reflect.MakeSlice(m.ReplyType.Elem(), 0, 0))
	}
	return replyv
}

type service struct {
	name   string // name of the stuct
	typ    reflect.Type
	rcvr   reflect.Value          // The instance of the structure itself. It is Saved because it will be needd as a parameter for the next call.
	method map[string]*methodType // store all methods that meet the criteria
}

func newService(rcvr interface{}) *service {
	s := new(service)
	s.rcvr = reflect.ValueOf(rcvr)
	// why there use reflect.Indirect(s.rcvr).Type instead of s.rcvr.Type
	// because if s.rcvr is a pointer, using s.rcvr.Type().Name() only return an empty string ""
	s.name = reflect.Indirect(s.rcvr).Type().Name()
	s.typ = reflect.TypeOf(rcvr)
	if !ast.IsExported(s.name) {
		log.Fatalf("rpc server: %s is not a valid service name: ", s.name)
	}
	s.registerMethods()
	return s
}

func (s *service) registerMethods() {
	s.method = make(map[string]*methodType)
	for i := 0; i < s.typ.NumMethod(); i++ {
		method := s.typ.Method(i)
		mType := method.Type
		if mType.NumIn() != 3 || mType.NumOut() != 1 {
			continue
		}
		if mType.Out(0) != reflect.TypeOf((*error)(nil)).Elem() {
			continue
		}
		argType, replyType := mType.In(1), mType.In(2)
		if !isExportedOrBuiltinType(argType) || !isExportedOrBuiltinType(replyType) {
			continue
		}
		s.method[method.Name] = &methodType{
			method:    method,
			ArgType:   argType,
			ReplyType: replyType,
		}
		log.Printf("rpc server: register %s.%s\n", s.name, method.Name)

	}
}

func isExportedOrBuiltinType(t reflect.Type) bool {
	return ast.IsExported(t.Name()) || t.PkgPath() == ""
}

func (s *service) call(m *methodType, argv, reply reflect.Value) error {
	atomic.AddUint64(&m.numCalls, 1)
	f := m.method.Func
	returnValues := f.Call([]reflect.Value{s.rcvr, argv, reply})
	if errInter := returnValues[0].Interface(); errInter != nil {
		return errInter.(error)
	}
	return nil
}

const (
	connected        = "200 Connected to Gee RPC"
	defaultRPCPath   = "/_geerpc_"
	defaultDebugPath = "/debug/geerpc"
)

// ServeHTTP implements an http.Handler that answers RPC requests.
func (server *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodConnect {
		w.Header().Set("Content-type", "text/plain;charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = io.WriteString(w, "405 must CONNECT\n")
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
	}

	coon, _, err := hijacker.Hijack()
	if err != nil {
		log.Printf("rpc hijacking %s: %s", req.RemoteAddr, err.Error())
		return
	}

	_, _ = io.WriteString(coon, "HTTP/1.0 "+connected+"\r\n\r\n")
	server.ServeConn(coon)
}

// HandleHTTP registers an HTTP handler for RPC messages on rpcPath.
// It is still necessary to invoke http.Serve(), typically in a go statement.
func (server *Server) HandleHTTP() {
	http.Handle(defaultRPCPath, server)
	http.Handle(defaultDebugPath, debugHTTP{server})
	log.Printf("rpc server debug path:%s", defaultDebugPath)
}

// HandleHTTP is a convenient approach for default server to register HTTP handlers
func HandleHTTP() {
	DefaultServer.HandleHTTP()
}
