/*
Copyright 2015 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package sshutils

import (
	"crypto/subtle"
	"fmt"
	"net"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/gravitational/teleport/lib/limiter"
	"github.com/gravitational/teleport/lib/utils"
	"golang.org/x/crypto/ssh"
)

type Server struct {
	addr           utils.NetAddr
	l              net.Listener
	closeC         chan struct{}
	newChanHandler NewChanHandler
	reqHandler     RequestHandler
	cfg            ssh.ServerConfig
	limiter        *limiter.Limiter
}

type ServerOption func(cfg *Server) error

func NewServer(a utils.NetAddr, h NewChanHandler, hostSigners []ssh.Signer,
	ah AuthMethods, limiter *limiter.Limiter, opts ...ServerOption) (*Server, error) {
	if err := checkArguments(a, h, hostSigners, ah); err != nil {
		return nil, err
	}
	s := &Server{
		addr:           a,
		newChanHandler: h,
		closeC:         make(chan struct{}),
		limiter:        limiter,
	}
	for _, o := range opts {
		if err := o(s); err != nil {
			return nil, err
		}
	}
	for _, signer := range hostSigners {
		(&s.cfg).AddHostKey(signer)
	}
	s.cfg.PublicKeyCallback = ah.PublicKey
	s.cfg.PasswordCallback = ah.Password
	s.cfg.NoClientAuth = ah.NoClient
	return s, nil
}

func SetSSHConfig(cfg ssh.ServerConfig) ServerOption {
	return func(s *Server) error {
		s.cfg = cfg
		return nil
	}
}

func SetRequestHandler(req RequestHandler) ServerOption {
	return func(s *Server) error {
		s.reqHandler = req
		return nil
	}
}

func (s *Server) Addr() string {
	return s.l.Addr().String()
}

func (s *Server) Start() error {
	socket, err := net.Listen(s.addr.AddrNetwork, s.addr.Addr)
	if err != nil {
		return err
	}
	s.l = socket
	log.Infof("created listening socket: %v", socket.Addr())
	go s.acceptConnections()
	return nil
}

func (s *Server) notifyClosed() {
	close(s.closeC)
}

func (s *Server) Wait() {
	<-s.closeC
}

// Close closes listening socket and stops accepting connections
func (s *Server) Close() error {
	return s.l.Close()
}

func (s *Server) acceptConnections() {
	defer s.notifyClosed()
	log.Infof("%v ready to accept connections", s.Addr())
	for {
		conn, err := s.l.Accept()
		if err != nil {
			// our best shot to avoid excessive logging
			if op, ok := err.(*net.OpError); ok && !op.Timeout() {
				log.Infof("socket closed: %v", op)
				return
			}
			log.Infof("accept error: %T %v", err, err)
			return
		}
		log.Infof("%v accepted connection from %v", s.Addr(), conn.RemoteAddr())

		go s.handleConnection(conn)
	}
}

func (s *Server) handleConnection(conn net.Conn) {
	// initiate an SSH connection, note that we don't need to close the conn here
	// in case of error as ssh server takes care of this

	remoteAddr, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		log.Errorf(err.Error())
	}
	if err := s.limiter.AcquireConnection(remoteAddr); err != nil {
		log.Errorf(err.Error())
		conn.Close()
		return
	}
	defer s.limiter.ReleaseConnection(remoteAddr)

	// setting waiting deadline in case of connection freezing
	err = conn.SetDeadline(time.Now().Add(5 * time.Minute))
	if err != nil {
		log.Errorf(err.Error())
	}
	sconn, chans, reqs, err := ssh.NewServerConn(conn, &s.cfg)
	if err != nil {
		log.Infof("failed to initiate connection, err: %v", err)
		conn.SetDeadline(time.Time{})
		return
	}
	err = conn.SetDeadline(time.Time{})
	if err != nil {
		log.Errorf(err.Error())
	}

	user := sconn.User()
	if err := s.limiter.RegisterRequest(user); err != nil {
		log.Errorf(err.Error())
		sconn.Close()
		conn.Close()
		return
	}
	// Connection successfully initiated
	log.Infof("new ssh connection %v -> %v vesion: %v",
		sconn.RemoteAddr(), sconn.LocalAddr(), string(sconn.ClientVersion()))

	wg := sync.WaitGroup{}
	wg.Add(2)

	go func() {
		// Handle incoming out-of-band Requests
		s.handleRequests(reqs)
		wg.Done()
	}()
	go func() {
		// Handle channel requests on this connections
		s.handleChannels(sconn, chans)
		wg.Done()
	}()

	wg.Wait()
}

func (s *Server) handleRequests(reqs <-chan *ssh.Request) {
	for req := range reqs {
		log.Infof("recieved out-of-band request: %+v", req)
		if s.reqHandler != nil {
			s.reqHandler.HandleRequest(req)
		}
	}
}

func (s *Server) handleChannels(sconn *ssh.ServerConn, chans <-chan ssh.NewChannel) {
	for nch := range chans {
		s.newChanHandler.HandleNewChan(sconn, nch)
	}
}

type RequestHandler interface {
	HandleRequest(r *ssh.Request)
}

type RequestHandlerFunc func(*ssh.Request)

func (f RequestHandlerFunc) HandleRequest(r *ssh.Request) {
	f(r)
}

type NewChanHandler interface {
	HandleNewChan(*ssh.ServerConn, ssh.NewChannel)
}

type NewChanHandlerFunc func(*ssh.ServerConn, ssh.NewChannel)

func (f NewChanHandlerFunc) HandleNewChan(s *ssh.ServerConn, c ssh.NewChannel) {
	f(s, c)
}

type AuthMethods struct {
	PublicKey PublicKeyFunc
	Password  PasswordFunc
	NoClient  bool
}

func checkArguments(a utils.NetAddr, h NewChanHandler, hostSigners []ssh.Signer, ah AuthMethods) error {
	if a.Addr == "" || a.AddrNetwork == "" {
		return fmt.Errorf("specify network and the address for listening socket")
	}
	if h == nil {
		return fmt.Errorf("specify new channel handler")
	}
	if len(hostSigners) == 0 {
		return fmt.Errorf("specify at least one host signer")
	}
	if ah.PublicKey == nil && ah.Password == nil && ah.NoClient == false {
		return fmt.Errorf("specify one method of auth or set NoClientAuth")
	}
	return nil
}

type PublicKeyFunc func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error)
type PasswordFunc func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error)

// KeysEqual is constant time compare of the keys to avoid timing attacks
func KeysEqual(ak, bk ssh.PublicKey) bool {
	a := ssh.Marshal(ak)
	b := ssh.Marshal(bk)
	return (len(a) == len(b) && subtle.ConstantTimeCompare(a, b) == 1)
}
