package main

import (
	"crypto/md5"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/godbus/dbus/v5"
)

const (
	mprisPath            = dbus.ObjectPath("/org/mpris/MediaPlayer2")
	mprisPlayerInterface = "org.mpris.MediaPlayer2.Player"
	mprisRootInterface   = "org.mpris.MediaPlayer2"
)

type playerControlAction int

const (
	controlNext playerControlAction = iota
	controlPrevious
	controlPause
	controlPlay
	controlQuit
)

type playerControl struct {
	action playerControlAction
}

type mprisService struct {
	conn     *dbus.Conn
	controls chan<- playerControl

	mu      sync.RWMutex
	track   Track
	playing bool
	volume  int
}

func startMPRIS(controls chan<- playerControl) (*mprisService, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}

	service := &mprisService{conn: conn, controls: controls, volume: 70}
	reply, err := conn.RequestName("org.mpris.MediaPlayer2.music", dbus.NameFlagDoNotQueue)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		conn.Close()
		return nil, errors.New("another MPRIS player is already using the music service name")
	}
	if err := conn.Export(service, mprisPath, mprisRootInterface); err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.Export(service, mprisPath, mprisPlayerInterface); err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.Export(service, mprisPath, "org.freedesktop.DBus.Properties"); err != nil {
		conn.Close()
		return nil, err
	}
	return service, nil
}

func (s *mprisService) stop() {
	if s != nil && s.conn != nil {
		s.conn.Close()
	}
}

func (s *mprisService) update(track Track, playing bool, volume int) {
	s.mu.Lock()
	s.track = track
	s.playing = playing
	s.volume = volume
	properties := s.playerPropertiesLocked()
	s.mu.Unlock()

	if err := s.conn.Emit(mprisPath, "org.freedesktop.DBus.Properties.PropertiesChanged", mprisPlayerInterface, properties, []string{}); err != nil {
		fmt.Fprintln(os.Stderr, "MPRIS update failed:", err)
	}
}

func (s *mprisService) sendControl(action playerControlAction) {
	select {
	case s.controls <- playerControl{action: action}:
	default:
	}
}

func (s *mprisService) Raise() *dbus.Error {
	return nil
}

func (s *mprisService) Quit() *dbus.Error {
	s.sendControl(controlQuit)
	return nil
}

func (s *mprisService) Next() *dbus.Error {
	s.sendControl(controlNext)
	return nil
}

func (s *mprisService) Previous() *dbus.Error {
	s.sendControl(controlPrevious)
	return nil
}

func (s *mprisService) Pause() *dbus.Error {
	s.sendControl(controlPause)
	return nil
}

func (s *mprisService) Play() *dbus.Error {
	s.sendControl(controlPlay)
	return nil
}

func (s *mprisService) PlayPause() *dbus.Error {
	s.mu.RLock()
	playing := s.playing
	s.mu.RUnlock()
	if playing {
		s.sendControl(controlPause)
	} else {
		s.sendControl(controlPlay)
	}
	return nil
}

func (s *mprisService) Stop() *dbus.Error {
	return s.Pause()
}

func (s *mprisService) Seek(int64) *dbus.Error {
	return dbus.MakeFailedError(errors.New("seeking is not supported"))
}

func (s *mprisService) SetPosition(dbus.ObjectPath, int64) *dbus.Error {
	return dbus.MakeFailedError(errors.New("seeking is not supported"))
}

func (s *mprisService) OpenUri(string) *dbus.Error {
	return dbus.MakeFailedError(errors.New("opening URIs is not supported"))
}

func (s *mprisService) Get(interfaceName, propertyName string) (dbus.Variant, *dbus.Error) {
	properties, err := s.properties(interfaceName)
	if err != nil {
		return dbus.Variant{}, err
	}
	property, ok := properties[propertyName]
	if !ok {
		return dbus.Variant{}, dbus.MakeFailedError(fmt.Errorf("unknown property %s", propertyName))
	}
	return property, nil
}

func (s *mprisService) GetAll(interfaceName string) (map[string]dbus.Variant, *dbus.Error) {
	return s.properties(interfaceName)
}

func (s *mprisService) Set(string, string, dbus.Variant) *dbus.Error {
	return dbus.MakeFailedError(errors.New("MPRIS properties are read-only"))
}

func (s *mprisService) properties(interfaceName string) (map[string]dbus.Variant, *dbus.Error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	switch interfaceName {
	case mprisRootInterface:
		return map[string]dbus.Variant{
			"CanQuit":             dbus.MakeVariant(true),
			"CanRaise":            dbus.MakeVariant(false),
			"HasTrackList":        dbus.MakeVariant(false),
			"Identity":            dbus.MakeVariant("music"),
			"SupportedUriSchemes": dbus.MakeVariant([]string{}),
			"SupportedMimeTypes":  dbus.MakeVariant([]string{}),
		}, nil
	case mprisPlayerInterface:
		return s.playerPropertiesLocked(), nil
	default:
		return nil, dbus.MakeFailedError(fmt.Errorf("unknown interface %s", interfaceName))
	}
}

func (s *mprisService) playerPropertiesLocked() map[string]dbus.Variant {
	status := "Stopped"
	if s.track.ID != "" {
		status = "Paused"
	}
	if s.playing {
		status = "Playing"
	}

	return map[string]dbus.Variant{
		"PlaybackStatus": dbus.MakeVariant(status),
		"LoopStatus":     dbus.MakeVariant("None"),
		"Rate":           dbus.MakeVariant(1.0),
		"Shuffle":        dbus.MakeVariant(true),
		"Metadata":       dbus.MakeVariant(mprisMetadata(s.track)),
		"Volume":         dbus.MakeVariant(float64(s.volume) / 100),
		"Position":       dbus.MakeVariant(int64(0)),
		"MinimumRate":    dbus.MakeVariant(1.0),
		"MaximumRate":    dbus.MakeVariant(1.0),
		"CanGoNext":      dbus.MakeVariant(true),
		"CanGoPrevious":  dbus.MakeVariant(true),
		"CanPlay":        dbus.MakeVariant(true),
		"CanPause":       dbus.MakeVariant(true),
		"CanSeek":        dbus.MakeVariant(false),
		"CanControl":     dbus.MakeVariant(true),
	}
}

func mprisMetadata(track Track) map[string]dbus.Variant {
	if track.ID == "" {
		return map[string]dbus.Variant{}
	}

	id := md5.Sum([]byte(track.ID))
	metadata := map[string]dbus.Variant{
		"mpris:trackid": dbus.MakeVariant(dbus.ObjectPath(fmt.Sprintf("/org/mpris/MediaPlayer2/track/%x", id))),
		"xesam:title":   dbus.MakeVariant(track.Title),
	}
	if track.Artist != "" {
		metadata["xesam:artist"] = dbus.MakeVariant([]string{track.Artist})
	}
	if track.Album != "" {
		metadata["xesam:album"] = dbus.MakeVariant(track.Album)
	}
	if track.Duration > 0 {
		metadata["mpris:length"] = dbus.MakeVariant(int64(track.Duration) * 1_000_000)
	}
	return metadata
}
