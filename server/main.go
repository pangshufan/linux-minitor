package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"monitor/internal/configfile"
	"monitor/internal/transport"
	"monitor/protocol"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	var grpcAddr, webAddr, dbPath, cert, key, ca, webCert, webKey, webUser string
	var offlineAfter, retention time.Duration
	var tlsEnabled, webTLSEnabled bool
	var driver, authToken, password string
	flag.BoolVar(&tlsEnabled, "tls-enabled", true, "enable gRPC mutual TLS")
	flag.BoolVar(&webTLSEnabled, "web-tls-enabled", true, "enable HTTPS outside loopback (certificates enable HTTPS on loopback too)")
	flag.StringVar(&driver, "db-driver", "sqlite", "database driver: sqlite or postgres")
	flag.StringVar(&authToken, "auth-token", "", "optional shared Agent authentication token")
	flag.StringVar(&password, "web-password", "", "Web password; prefer config or MONITOR_WEB_PASSWORD")
	flag.StringVar(&grpcAddr, "grpc-listen", ":8443", "Agent gRPC listen address")
	flag.StringVar(&webAddr, "web-listen", "127.0.0.1:8080", "Web listen address")
	flag.StringVar(&dbPath, "db", "monitor.db", "SQLite file or PostgreSQL DSN")
	flag.StringVar(&cert, "tls-cert", "", "gRPC server certificate")
	flag.StringVar(&key, "tls-key", "", "gRPC server private key")
	flag.StringVar(&ca, "tls-ca", "", "trusted Agent CA")
	flag.StringVar(&webCert, "web-cert", "", "Web HTTPS certificate")
	flag.StringVar(&webKey, "web-key", "", "Web HTTPS private key")
	flag.StringVar(&webUser, "web-user", "monitor", "Web Basic Auth username")
	flag.DurationVar(&offlineAfter, "offline-after", 15*time.Second, "Agent heartbeat timeout")
	flag.DurationVar(&retention, "retention", 7*24*time.Hour, "history retention")
	if err := configfile.Parse(flag.CommandLine, os.Args[1:], map[string]string{"web-password": "MONITOR_WEB_PASSWORD", "auth-token": "MONITOR_AUTH_TOKEN", "db": "MONITOR_DB"}); err != nil {
		return err
	}
	if password == "" || webUser == "" {
		return errors.New("set web-user and web-password (or MONITOR_WEB_PASSWORD)")
	}
	if offlineAfter <= time.Second || retention <= 0 {
		return errors.New("offline timeout must exceed 1s and retention must be positive")
	}
	if tlsEnabled && (cert == "" || key == "" || ca == "") {
		return errors.New("-tls-cert, -tls-key and -tls-ca are required")
	}
	host, _, err := net.SplitHostPort(webAddr)
	if err != nil {
		return err
	}
	if webTLSEnabled && ((webCert == "") != (webKey == "") || (!net.ParseIP(host).IsLoopback() && webCert == "")) {
		return errors.New("set both -web-cert and -web-key for Web access outside the loopback address")
	}
	grpcOptions := []grpc.ServerOption{grpc.MaxRecvMsgSize(transport.MaxMessageBytes), grpc.MaxSendMsgSize(transport.MaxMessageBytes)}
	if tlsEnabled {
		tlsConfig, err := transport.TLSConfig(cert, key, ca)
		if err != nil {
			return err
		}
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
		grpcOptions = append(grpcOptions, grpc.Creds(credentials.NewTLS(tlsConfig)))
	}

	listener, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		return err
	}
	defer listener.Close()
	storage, err := openDatabase(driver, dbPath)
	if err != nil {
		return err
	}
	defer storage.db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	h := &hub{store: storage, offlineAfter: offlineAfter, sessions: make(map[string]*session), plaintext: !tlsEnabled, authToken: authToken}
	gs := grpc.NewServer(grpcOptions...)
	protocol.RegisterMonitorServer(gs, h)
	defer gs.Stop()
	web := &http.Server{
		Addr: webAddr, Handler: h.webRoutes(webUser, password),
		ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	defer web.Close()

	fail := make(chan error, 2)
	go func() { fail <- gs.Serve(listener) }()
	go func() {
		if webTLSEnabled && webCert != "" {
			fail <- web.ListenAndServeTLS(webCert, webKey)
		} else {
			fail <- web.ListenAndServe()
		}
	}()
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for ctx.Err() == nil {
			if err := storage.prune(ctx, time.Now().Add(-retention)); err != nil && ctx.Err() == nil {
				log.Printf("prune history: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	log.Printf("server gRPC=%s tls=%t web=%s database_driver=%s", listener.Addr(), tlsEnabled, webAddr, driver)
	select {
	case err := <-fail:
		return err
	case <-ctx.Done():
		gs.Stop()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return web.Shutdown(shutdown)
	}
}
