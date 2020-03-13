package kirin

import (
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/lightninglabs/kirin/auth"
	"github.com/lightninglabs/kirin/mint"
	"github.com/lightninglabs/kirin/proxy"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/cert"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/tor"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"gopkg.in/yaml.v2"
)

const (
	// topLevelKey is the top level key for an etcd cluster where we'll
	// store all LSAT proxy related data.
	topLevelKey = "lsat/proxy"

	// etcdKeyDelimeter is the delimeter we'll use for all etcd keys to
	// represent a path-like structure.
	etcdKeyDelimeter = "/"
)

// Main is the true entrypoint of Kirin.
func Main() {
	// TODO: Prevent from running twice.
	err := start()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// start sets up the proxy server and runs it. This function blocks until a
// shutdown signal is received.
func start() error {
	// First, parse configuration file and set up logging.
	configFile := filepath.Join(kirinDataDir, defaultConfigFilename)
	cfg, err := getConfig(configFile)
	if err != nil {
		return fmt.Errorf("unable to parse config file: %v", err)
	}
	err = setupLogging(cfg)
	if err != nil {
		return fmt.Errorf("unable to set up logging: %v", err)
	}

	// Initialize our etcd client.
	etcdClient, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{cfg.Etcd.Host},
		DialTimeout: 5 * time.Second,
		Username:    cfg.Etcd.User,
		Password:    cfg.Etcd.Password,
	})
	if err != nil {
		return fmt.Errorf("unable to connect to etcd: %v", err)
	}

	// Create the proxy and connect it to lnd.
	genInvoiceReq := func() (*lnrpc.Invoice, error) {
		return &lnrpc.Invoice{
			Memo:  "LSAT",
			Value: 1,
		}, nil
	}
	servicesProxy, err := createProxy(cfg, genInvoiceReq, etcdClient)
	if err != nil {
		return err
	}
	handler := http.HandlerFunc(servicesProxy.ServeHTTP)
	httpsServer := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: handler,
	}

	// Create TLS certificates.
	var tlsKeyFile, tlsCertFile string
	switch {
	// When using autocert, we set a TLSConfig on the server so the key and
	// cert file we pass in are ignored and don't need to exist.
	case cfg.AutoCert:
		serverName := cfg.ServerName
		if serverName == "" {
			return fmt.Errorf("servername option is required for " +
				"secure operation")
		}

		certDir := filepath.Join(kirinDataDir, "autocert")
		log.Infof("Configuring autocert for server %v with cache dir "+
			"%v", serverName, certDir)

		manager := autocert.Manager{
			Cache:      autocert.DirCache(certDir),
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(serverName),
		}

		go func() {
			err := http.ListenAndServe(
				":http", manager.HTTPHandler(nil),
			)
			if err != nil {
				log.Errorf("autocert http: %v", err)
			}
		}()
		httpsServer.TLSConfig = &tls.Config{
			GetCertificate: manager.GetCertificate,
		}

	// If we're not using autocert, we want to create self-signed TLS certs
	// and save them at the specified location (if they don't already
	// exist).
	default:
		tlsKeyFile = filepath.Join(kirinDataDir, defaultTLSKeyFilename)
		tlsCertFile = filepath.Join(kirinDataDir, defaultTLSCertFilename)
		if !fileExists(tlsCertFile) && !fileExists(tlsKeyFile) {
			log.Infof("Generating TLS certificates...")
			err := cert.GenCertPair(
				"kirin autogenerated cert", tlsCertFile,
				tlsKeyFile, nil, nil,
				cert.DefaultAutogenValidity,
			)
			if err != nil {
				return err
			}
			log.Infof("Done generating TLS certificates")
		}
	}

	// The ListenAndServeTLS below will block until shut down or an error
	// occurs. So we can just defer a cleanup function here that will close
	// everything on shutdown.
	defer cleanup(etcdClient, httpsServer)

	// Finally start the server.
	log.Infof("Starting the server, listening on %s.", cfg.ListenAddr)

	errChan := make(chan error)
	go func() {
		errChan <- httpsServer.ListenAndServeTLS(tlsCertFile, tlsKeyFile)
	}()

	// If we need to listen over Tor as well, we'll set up the onion
	// services now. We're not able to use TLS for onion services since they
	// can't be verified, so we'll spin up an additional HTTP/2 server
	// _without_ TLS that is not exposed to the outside world. This server
	// will only be reached through the onion services, which already
	// provide encryption, so running this additional HTTP server should be
	// relatively safe.
	if cfg.Tor.V2 || cfg.Tor.V3 {
		torController, err := initTorListener(cfg, etcdClient)
		if err != nil {
			return err
		}
		defer func() {
			_ = torController.Stop()
		}()

		httpServer := &http.Server{
			Addr:    fmt.Sprintf("localhost:%d", cfg.Tor.ListenPort),
			Handler: h2c.NewHandler(handler, &http2.Server{}),
		}
		go func() {
			errChan <- httpServer.ListenAndServe()
		}()
		defer httpServer.Close()
	}

	return <-errChan
}

// fileExists reports whether the named file or directory exists.
// This function is taken from https://github.com/btcsuite/btcd
func fileExists(name string) bool {
	if _, err := os.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// getConfig loads and parses the configuration file then checks it for valid
// content.
func getConfig(configFile string) (*config, error) {
	cfg := &config{}
	b, err := ioutil.ReadFile(configFile)
	if err != nil {
		return nil, err
	}
	err = yaml.Unmarshal(b, cfg)
	if err != nil {
		return nil, err
	}

	// Then check the configuration that we got from the config file, all
	// required values need to be set at this point.
	if cfg.ListenAddr == "" {
		return nil, fmt.Errorf("missing listen address for server")
	}
	return cfg, nil
}

// setupLogging parses the debug level and initializes the log file rotator.
func setupLogging(cfg *config) error {
	if cfg.DebugLevel == "" {
		cfg.DebugLevel = defaultLogLevel
	}

	// Now initialize the logger and set the log level.
	logFile := filepath.Join(kirinDataDir, defaultLogFilename)
	err := logWriter.InitLogRotator(
		logFile, defaultMaxLogFileSize, defaultMaxLogFiles,
	)
	if err != nil {
		return err
	}
	return build.ParseAndSetDebugLevels(cfg.DebugLevel, logWriter)
}

// initTorListener initiates a Tor controller instance with the Tor server
// specified in the config. Onion services will be created over which the proxy
// can be reached at.
func initTorListener(cfg *config, etcd *clientv3.Client) (*tor.Controller, error) {
	// Establish a controller connection with the backing Tor server and
	// proceed to create the requested onion services.
	onionCfg := tor.AddOnionConfig{
		VirtualPort: int(cfg.Tor.VirtualPort),
		TargetPorts: []int{int(cfg.Tor.ListenPort)},
		Store:       newOnionStore(etcd),
	}
	torController := tor.NewController(cfg.Tor.Control, "", "")
	if err := torController.Start(); err != nil {
		return nil, err
	}

	if cfg.Tor.V2 {
		onionCfg.Type = tor.V2
		addr, err := torController.AddOnion(onionCfg)
		if err != nil {
			return nil, err
		}

		log.Infof("Listening over Tor on %v", addr)
	}

	if cfg.Tor.V3 {
		onionCfg.Type = tor.V3
		addr, err := torController.AddOnion(onionCfg)
		if err != nil {
			return nil, err
		}

		log.Infof("Listening over Tor on %v", addr)
	}

	return torController, nil
}

// createProxy creates the proxy with all the services it needs.
func createProxy(cfg *config, genInvoiceReq InvoiceRequestGenerator,
	etcdClient *clientv3.Client) (*proxy.Proxy, error) {

	challenger, err := NewLndChallenger(cfg.Authenticator, genInvoiceReq)
	if err != nil {
		return nil, err
	}
	minter := mint.New(&mint.Config{
		Challenger:     challenger,
		Secrets:        newSecretStore(etcdClient),
		ServiceLimiter: newStaticServiceLimiter(cfg.Services),
	})
	authenticator := auth.NewLsatAuthenticator(minter)
	return proxy.New(authenticator, cfg.Services, cfg.StaticRoot)
}

// cleanup closes the given server and shuts down the log rotator.
func cleanup(etcdClient io.Closer, server io.Closer) {
	if err := etcdClient.Close(); err != nil {
		log.Errorf("Error terminating etcd client: %v", err)
	}
	err := server.Close()
	if err != nil {
		log.Errorf("Error closing server: %v", err)
	}
	log.Info("Shutdown complete")
	err = logWriter.Close()
	if err != nil {
		log.Errorf("Could not close log rotator: %v", err)
	}
}
