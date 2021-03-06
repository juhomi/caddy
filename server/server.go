// Package server implements a configurable, general-purpose web server.
// It relies on configurations obtained from the adjacent config package
// and can execute middleware as defined by the adjacent middleware package.
package server

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"

	"github.com/bradfitz/http2"
	"github.com/mholt/caddy/config"
)

// Server represents an instance of a server, which serves
// static content at a particular address (host and port).
type Server struct {
	HTTP2   bool                   // temporary while http2 is not in std lib (TODO: remove flag when part of std lib)
	address string                 // the actual address for net.Listen to listen on
	tls     bool                   // whether this server is serving all HTTPS hosts or not
	vhosts  map[string]virtualHost // virtual hosts keyed by their address
}

// New creates a new Server which will bind to addr and serve
// the sites/hosts configured in configs. This function does
// not start serving.
func New(addr string, configs []config.Config, tls bool) (*Server, error) {
	s := &Server{
		address: addr,
		tls:     tls,
		vhosts:  make(map[string]virtualHost),
	}

	for _, conf := range configs {
		if _, exists := s.vhosts[conf.Host]; exists {
			return nil, fmt.Errorf("Cannot serve %s - host already defined for address %s", conf.Address(), s.address)
		}

		vh := virtualHost{config: conf}

		// Build middleware stack
		err := vh.buildStack()
		if err != nil {
			return nil, err
		}

		s.vhosts[conf.Host] = vh
	}

	return s, nil
}

// Serve starts the server. It blocks until the server quits.
func (s *Server) Serve() error {
	server := &http.Server{
		Addr:    s.address,
		Handler: s,
	}

	if s.HTTP2 {
		// TODO: This call may not be necessary after HTTP/2 is merged into std lib
		http2.ConfigureServer(server, nil)
	}

	for _, vh := range s.vhosts {
		// Execute startup functions now
		for _, start := range vh.config.Startup {
			err := start()
			if err != nil {
				return err
			}
		}

		// Execute shutdown commands on exit
		if len(vh.config.Shutdown) > 0 {
			go func() {
				interrupt := make(chan os.Signal, 1)
				signal.Notify(interrupt, os.Interrupt, os.Kill) // TODO: syscall.SIGQUIT? (Ctrl+\, Unix-only)
				<-interrupt
				for _, shutdownFunc := range vh.config.Shutdown {
					err := shutdownFunc()
					if err != nil {
						log.Fatal(err)
					}
				}
				os.Exit(0)
			}()
		}
	}

	if s.tls {
		var tlsConfigs []config.TLSConfig
		for _, vh := range s.vhosts {
			tlsConfigs = append(tlsConfigs, vh.config.TLS)
		}
		return ListenAndServeTLSWithSNI(server, tlsConfigs)
	} else {
		return server.ListenAndServe()
	}
}

// ListenAndServeTLSWithSNI serves TLS with Server Name Indication (SNI) support, which allows
// multiple sites (different hostnames) to be served from the same address. This method is
// adapted directly from the std lib's net/http ListenAndServeTLS function, which was
// written by the Go Authors. It has been modified to support multiple certificate/key pairs.
func ListenAndServeTLSWithSNI(srv *http.Server, tlsConfigs []config.TLSConfig) error {
	addr := srv.Addr
	if addr == "" {
		addr = ":https"
	}

	config := new(tls.Config)
	if srv.TLSConfig != nil {
		*config = *srv.TLSConfig
	}
	if config.NextProtos == nil {
		config.NextProtos = []string{"http/1.1"}
	}

	// Here we diverge from the stdlib a bit by loading multiple certs/key pairs
	// then we map the server names to their certs
	var err error
	config.Certificates = make([]tls.Certificate, len(tlsConfigs))
	for i, tlsConfig := range tlsConfigs {
		config.Certificates[i], err = tls.LoadX509KeyPair(tlsConfig.Certificate, tlsConfig.Key)
		if err != nil {
			return err
		}
	}
	config.BuildNameToCertificate()

	conn, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	tlsListener := tls.NewListener(conn, config)
	return srv.Serve(tlsListener)
}

// ServeHTTP is the entry point for every request to the address that s
// is bound to. It acts as a multiplexer for the requests hostname as
// defined in the Host header so that the correct virtualhost
// (configuration and middleware stack) will handle the request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		// In case the user doesn't enable error middleware, we still
		// need to make sure that we stay alive up here
		if rec := recover(); rec != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError),
				http.StatusInternalServerError)
		}
	}()

	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host // oh well
	}

	if vh, ok := s.vhosts[host]; ok {
		w.Header().Set("Server", "Caddy")

		status, _ := vh.stack.ServeHTTP(w, r)

		// Fallback error response in case error handling wasn't chained in
		if status >= 400 {
			w.WriteHeader(status)
			fmt.Fprintf(w, "%d %s", status, http.StatusText(status))
		}
	} else {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "No such host at %s", s.address)
	}
}
