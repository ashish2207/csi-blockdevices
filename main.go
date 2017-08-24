package main

import (
	"errors"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"

	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	"github.com/codedellemc/gocsi"
	"github.com/codedellemc/gocsi/csi"
)

const (
	spName = "csi-blockdevices"

	nodeEnvVar     = "BDPLUGIN_NODEONLY"
	ctlrEnvVar     = "BDPLUGIN_CONTROLLERONLY"
	debugEnvVar    = "BDPLUGIN_DEBUG"
	blockDirEnvVar = "BDPLUGIN_DEVDIR"

	defaultDevDir = "/dev/disk/csi-blockdevices"
)

var (
	errServerStarted = errors.New(spName + ": the server has been started")
	errServerStopped = errors.New(spName + ": the server has been stopped")

	DevDir  = defaultDevDir
	PrivDir = filepath.Join(DevDir, ".mounts")

	CSIVersions = []*csi.Version{
		&csi.Version{
			Major: 0,
			Minor: 1,
			Patch: 0,
		},
	}
)

func main() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, os.Kill)

	if _, d := os.LookupEnv(debugEnvVar); d {
		log.SetLevel(log.DebugLevel)
	}

	if dd := os.Getenv(blockDirEnvVar); dd != "" {
		DevDir = dd
		PrivDir = filepath.Join(DevDir, ".mounts")
	}

	s := &sp{name: spName}

	go func() {
		_ = <-c
		if s.server != nil {
			s.Lock()
			defer s.Unlock()
			log.Info("Shutting down server")
			s.server.GracefulStop()
			s.closed = true

			// make sure sock file got cleaned up
			proto, addr, _ := gocsi.GetCSIEndpoint()
			if proto == "unix" && addr != "" {
				if _, err := os.Stat(addr); !os.IsNotExist(err) {
					s.server.Stop()
					if err := os.Remove(addr); err != nil {
						log.WithError(err).Warn(
							"Unable to remove sock file")
					}
				}
			}
		}
	}()

	l, err := gocsi.GetCSIEndpointListener()
	if err != nil {
		log.WithError(err).Fatal("failed to listen")
	}

	ctx := context.Background()

	if err := s.Serve(ctx, l); err != nil {
		s.Lock()
		defer s.Unlock()
		if !s.closed {
			log.WithError(err).Fatal("grpc failed")
		}
	}
}

type sp struct {
	sync.Mutex
	name   string
	server *grpc.Server
	closed bool
}

// ServiceProvider.Serve
func (s *sp) Serve(ctx context.Context, li net.Listener) error {
	log.WithField("name", s.name).Info(".Serve")
	if err := func() error {
		s.Lock()
		defer s.Unlock()
		if s.closed {
			return errServerStopped
		}
		if s.server != nil {
			return errServerStarted
		}
		s.server = grpc.NewServer(grpc.UnaryInterceptor(gocsi.ChainUnaryServer(
			gocsi.ServerRequestIDInjector,
			gocsi.NewServerRequestLogger(os.Stdout, os.Stderr),
			gocsi.NewServerResponseLogger(os.Stdout, os.Stderr),
			gocsi.NewServerRequestVersionValidator(CSIVersions),
			gocsi.ServerRequestValidator)))
		return nil
	}(); err != nil {
		return errServerStarted
	}

	// Always host the Indentity Service
	csi.RegisterIdentityServer(s.server, s)

	_, nodeSvc := os.LookupEnv(nodeEnvVar)
	_, ctrlSvc := os.LookupEnv(ctlrEnvVar)

	if nodeSvc && ctrlSvc {
		log.Fatalf("Cannot specify both %s and %s",
			nodeEnvVar, ctlrEnvVar)
	}

	switch {
	case nodeSvc:
		csi.RegisterNodeServer(s.server, s)
		log.Debug("Added Node Service")
	case ctrlSvc:
		csi.RegisterControllerServer(s.server, s)
		log.Debug("Added Controller Service")
	default:
		csi.RegisterControllerServer(s.server, s)
		log.Debug("Added Controller Service")
		csi.RegisterNodeServer(s.server, s)
		log.Debug("Added Node Service")
	}

	// start the grpc server
	if err := s.server.Serve(li); err != grpc.ErrServerStopped {
		return err
	}
	return errServerStopped
}