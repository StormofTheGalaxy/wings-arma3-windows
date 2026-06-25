package server

import (
	"context"
	"time"

	"emperror.dev/errors"

	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/remote"
)

type environmentInstaller interface {
	Install(context.Context) error
}

// Install executes the installation stack for a server process. Bubbles any
// errors up to the calling function which should handle contacting the panel to
// notify it of the server state.
//
// Pass true as the first argument in order to execute a server sync before the
// process to ensure the latest information is used.
func (s *Server) Install() error {
	return s.install(false)
}

func (s *Server) install(reinstall bool) error {
	var err error
	_, hasNativeInstaller := s.Environment.(environmentInstaller)
	if !s.Config().SkipEggScripts || hasNativeInstaller {
		// Send the start event so the Panel can automatically update. We don't
		// send this unless the process is actually going to run, otherwise all
		// sorts of weird rapid UI behavior happens since there isn't an actual
		// install process being executed.
		s.Events().Publish(InstallStartedEvent, "")

		err = s.internalInstall()
	} else {
		s.Log().Info("server configured to skip running installation scripts for this egg, not executing process")
	}

	s.Log().WithField("was_successful", err == nil).Debug("notifying panel of server install state")
	if serr := s.SyncInstallState(err == nil, reinstall); serr != nil {
		l := s.Log().WithField("was_successful", err == nil)

		// If the request was successful but there was an error with this request,
		// attach the error to this log entry. Otherwise, ignore it in this log
		// since whatever is calling this function should handle the error and
		// will end up logging the same one.
		if err == nil {
			l.WithField("error", err)
		}

		l.Warn("failed to notify panel of server install state")
	}

	// Ensure that the server is marked as offline at this point, otherwise you
	// end up with a blank value which is a bit confusing.
	s.Environment.SetState(environment.ProcessOfflineState)

	// Push an event to the websocket, so we can auto-refresh the information in
	// the panel once the installation is completed.
	s.Events().Publish(InstallCompletedEvent, "")

	return errors.WithStackIf(err)
}

// Reinstall reinstalls a server's software by utilizing the installation script
// for the server egg. This does not touch any existing files for the server,
// other than what the script modifies.
func (s *Server) Reinstall() error {
	if s.Environment.State() != environment.ProcessOfflineState {
		s.Log().Debug("waiting for server instance to enter a stopped state")
		if err := s.Environment.WaitForStop(s.Context(), time.Second*10, true); err != nil {
			return errors.WrapIf(err, "install: failed to stop running environment")
		}
	}

	s.Log().Info("syncing server state with remote source before executing re-installation process")
	if err := s.Sync(); err != nil {
		return errors.WrapIf(err, "install: failed to sync server state with Panel")
	}

	return s.install(true)
}

// Internal installation function used to simplify reporting back to the Panel.
func (s *Server) internalInstall() error {
	s.Log().Debug("acquiring installation process lock")
	if !s.installing.SwapIf(true) {
		return errors.New("install: cannot obtain installation lock")
	}
	defer func() {
		s.Log().Debug("releasing installation process lock")
		s.installing.Store(false)
	}()

	installer, ok := s.Environment.(environmentInstaller)
	if !ok {
		s.PublishConsoleOutputFromDaemon("Installer is not available for the configured environment.")
		return nil
	}
	s.PublishConsoleOutputFromDaemon("Starting Windows SteamCMD installation process...")
	return installer.Install(s.Context())
}

// IsInstalling returns if the server is actively running the installation
// process by checking the status of the installer lock.
func (s *Server) IsInstalling() bool {
	return s.installing.Load()
}

func (s *Server) IsTransferring() bool {
	return s.transferring.Load()
}

func (s *Server) SetTransferring(state bool) {
	s.transferring.Store(state)
}

func (s *Server) IsRestoring() bool {
	return s.restoring.Load()
}

func (s *Server) SetRestoring(state bool) {
	s.restoring.Store(state)
}

// SyncInstallState makes an HTTP request to the Panel instance notifying it that
// the server has completed the installation process, and what the state of the
// server is.
func (s *Server) SyncInstallState(successful, reinstall bool) error {
	return s.client.SetInstallationStatus(s.Context(), s.ID(), remote.InstallStatusRequest{
		Successful: successful,
		Reinstall:  reinstall,
	})
}
