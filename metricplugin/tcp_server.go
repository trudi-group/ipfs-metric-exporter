package metricplugin

import (
	"net"
	"strings"
	"sync"

	"github.com/pkg/errors"
)

type tcpServer struct {
	// The monitoring API of the plugin.
	monitor MonitoringAPI

	// A mutex to coordinate shutting down.
	closingLock sync.Mutex
	// A channel shared with all clients to notify them if we're shutting down.
	closing chan struct{}
	// A wait group to keep track of the clients.
	wg sync.WaitGroup

	// The underlying listeners.
	listeners []net.Listener
}

// TCPServerConfig is the config for the metric export TCP server.
type TCPServerConfig struct {
	// ListenAddresses specifies the addresses to listen on.
	// It should take a form usable by `net.ResolveTCPAddr`, for example
	// `localhost:8181` or `0.0.0.0:1444`.
	ListenAddresses []string `json:"ListenAddresses"`
}

func newTCPServer(monitor MonitoringAPI, cfg TCPServerConfig) (*tcpServer, error) {
	if len(cfg.ListenAddresses) == 0 {
		return nil, errors.New("missing ListenAddresses")
	}

	var listeners []net.Listener
	for _, addr := range cfg.ListenAddresses {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, errors.Wrap(err, "unable to bind")
		}
		log.Infof("metric export TCP API running on %s", listener.Addr())

		listeners = append(listeners, listener)
	}

	return &tcpServer{
		monitor:   monitor,
		listeners: listeners,
		closing:   make(chan struct{}),
		wg:        sync.WaitGroup{},
	}, nil
}

func (s *tcpServer) StartServer() {
	for _, listener := range s.listeners {
		s.wg.Add(1)
		go func(listener net.Listener) {
			defer s.wg.Done()
			for {
				// Accept a new connection.
				c, err := listener.Accept()
				if err != nil {
					// We search for this error string to figure out if we're
					// shutting down :)
					if !strings.Contains(err.Error(), "use of closed network connection") {
						log.Error("unable to accept connection: ", err)
					}
					return
				}
				log.Infof("new connection from %s", c.RemoteAddr())

				client, err := newTCPSubscriber(c, s.closing)
				if err != nil {
					log.Warnf("unable to set up client %s", c.RemoteAddr())
					continue
				}

				// Keep track of the connection and let it run.
				s.wg.Add(1)
				go func() {
					defer s.wg.Done()
					defer s.monitor.Unsubscribe(client)

					err := s.monitor.Subscribe(client)
					if err != nil {
						log.Errorf("duplicate TCP client? %s", err)
					}
					client.run()
				}()
			}
		}(listener)
	}
}

// Shutdown shuts down the TCP server and all connected clients.
// This will close the listeners as well as all currently open connections.
// This blocks until all connections are closed, but that shouldn't take long.
func (s *tcpServer) Shutdown() {
	s.closingLock.Lock()
	select {
	case <-s.closing:
		// Already closing/closed, no-op.
		s.closingLock.Unlock()
		return
	default:
		close(s.closing)
	}
	s.closingLock.Unlock()

	log.Info("shutting down TCP server")

	// Shut down listeners.
	for _, listener := range s.listeners {
		err := listener.Close()
		if err != nil {
			log.Errorw("unable to close TCP listener?", "err", err)
		}
	}

	// Wait for clients to shut down.
	s.wg.Wait()
	log.Info("TCP server shut down")
}
