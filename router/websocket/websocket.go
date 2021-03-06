package websocket

import (
	"fmt"
	"github.com/gbrlsnchs/jwt/v3"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pkg/errors"
	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/router/tokens"
	"github.com/pterodactyl/wings/server"
	"go.uber.org/zap"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var alg *jwt.HMACSHA

const (
	PermissionConnect          = "websocket.*"
	PermissionSendCommand      = "control.console"
	PermissionSendPowerStart   = "control.start"
	PermissionSendPowerStop    = "control.stop"
	PermissionSendPowerRestart = "control.restart"
	PermissionReceiveErrors    = "admin.errors"
	PermissionReceiveInstall   = "admin.install"
	PermissionReceiveBackups   = "backup.read"
)

type Handler struct {
	Connection *websocket.Conn
	JWT        *tokens.WebsocketPayload `json:"-"`
	server     *server.Server
	mutex      sync.Mutex
}

// Parses a JWT into a websocket token payload.
func NewTokenPayload(token []byte) (*tokens.WebsocketPayload, error) {
	payload := tokens.WebsocketPayload{}
	err := tokens.ParseToken(token, &payload)
	if err != nil {
		return nil, err
	}

	if !payload.HasPermission(PermissionConnect) {
		return nil, errors.New("not authorized to connect to this socket")
	}

	return &payload, nil
}

// Returns a new websocket handler using the context provided.
func GetHandler(s *server.Server, w http.ResponseWriter, r *http.Request) (*Handler, error) {
	upgrader := websocket.Upgrader{
		// Ensure that the websocket request is originating from the Panel itself,
		// and not some other location.
		CheckOrigin: func(r *http.Request) bool {
			return r.Header.Get("Origin") == config.Get().PanelLocation
		},
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return nil, err
	}

	return &Handler{
		Connection: conn,
		JWT:        nil,
		server:     s,
		mutex:      sync.Mutex{},
	}, nil
}

func (h *Handler) SendJson(v *Message) error {
	// Do not send JSON down the line if the JWT on the connection is not
	// valid!
	if err := h.TokenValid(); err != nil {
		return nil
	}

	// If we're sending installation output but the user does not have the required
	// permissions to see the output, don't send it down the line.
	if v.Event == server.InstallOutputEvent {
		zap.S().Debugf("%+v", v.Args)
		if h.JWT != nil && !h.JWT.HasPermission(PermissionReceiveInstall) {
			return nil
		}
	}

	// If the user does not have permission to see backup events, do not emit
	// them over the socket.
	if strings.HasPrefix(v.Event, server.BackupCompletedEvent) {
		if h.JWT != nil && !h.JWT.HasPermission(PermissionReceiveBackups) {
			return nil
		}
	}

	return h.unsafeSendJson(v)
}

// Sends JSON over the websocket connection, ignoring the authentication state of the
// socket user. Do not call this directly unless you are positive a response should be
// sent back to the client!
func (h *Handler) unsafeSendJson(v interface{}) error {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	return h.Connection.WriteJSON(v)
}

// Checks if the JWT is still valid.
func (h *Handler) TokenValid() error {
	if h.JWT == nil {
		return errors.New("no jwt present")
	}

	if err := jwt.ExpirationTimeValidator(time.Now())(&h.JWT.Payload); err != nil {
		return err
	}

	if !h.JWT.HasPermission(PermissionConnect) {
		return errors.New("jwt does not have connect permission")
	}

	if h.server.Uuid != h.JWT.ServerUUID {
		return errors.New("jwt server uuid mismatch")
	}

	return nil
}

// Sends an error back to the connected websocket instance by checking the permissions
// of the token. If the user has the "receive-errors" grant we will send back the actual
// error message, otherwise we just send back a standard error message.
func (h *Handler) SendErrorJson(err error) error {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	message := "an unexpected error was encountered while handling this request"
	if h.JWT != nil {
		if server.IsSuspendedError(err) || h.JWT.HasPermission(PermissionReceiveErrors) {
			message = err.Error()
		}
	}

	m, u := h.GetErrorMessage(message)

	wsm := Message{Event: ErrorEvent}
	wsm.Args = []string{m}

	if !server.IsSuspendedError(err) {
		zap.S().Errorw(
			"an error was encountered in the websocket process",
			zap.String("server", h.server.Uuid),
			zap.String("error_identifier", u.String()),
			zap.Error(err),
		)
	}

	return h.Connection.WriteJSON(wsm)
}

// Converts an error message into a more readable representation and returns a UUID
// that can be cross-referenced to find the specific error that triggered.
func (h *Handler) GetErrorMessage(msg string) (string, uuid.UUID) {
	u := uuid.Must(uuid.NewRandom())

	m := fmt.Sprintf("Error Event [%s]: %s", u.String(), msg)

	return m, u
}

// Handle the inbound socket request and route it to the proper server action.
func (h *Handler) HandleInbound(m Message) error {
	if m.Event != AuthenticationEvent {
		if err := h.TokenValid(); err != nil {
			zap.S().Debugw("jwt token is no longer valid", zap.String("message", err.Error()))

			h.unsafeSendJson(Message{
				Event: ErrorEvent,
				Args:  []string{"could not authenticate client: " + err.Error()},
			})

			return nil
		}
	}

	switch m.Event {
	case AuthenticationEvent:
		{
			token, err := NewTokenPayload([]byte(strings.Join(m.Args, "")))
			if err != nil {
				return err
			}

			if token.HasPermission(PermissionConnect) {
				h.JWT = token
			}

			// On every authentication event, send the current server status back
			// to the client. :)
			h.server.Events().Publish(server.StatusEvent, h.server.State)

			h.unsafeSendJson(Message{
				Event: AuthenticationSuccessEvent,
				Args:  []string{},
			})

			return nil
		}
	case SetStateEvent:
		{
			switch strings.Join(m.Args, "") {
			case "start":
				if h.JWT.HasPermission(PermissionSendPowerStart) {
					return h.server.Environment.Start()
				}
				break
			case "stop":
				if h.JWT.HasPermission(PermissionSendPowerStop) {
					return h.server.Environment.Stop()
				}
				break
			case "restart":
				if h.JWT.HasPermission(PermissionSendPowerRestart) {
					if err := h.server.Environment.WaitForStop(60, false); err != nil {
						return err
					}

					return h.server.Environment.Start()
				}
				break
			case "kill":
				if h.JWT.HasPermission(PermissionSendPowerStop) {
					return h.server.Environment.Terminate(os.Kill)
				}
				break
			}

			return nil
		}
	case SendServerLogsEvent:
		{
			if running, _ := h.server.Environment.IsRunning(); !running {
				return nil
			}

			logs, err := h.server.Environment.Readlog(1024 * 16)
			if err != nil {
				return err
			}

			for _, line := range logs {
				h.SendJson(&Message{
					Event: server.ConsoleOutputEvent,
					Args:  []string{line},
				})
			}

			return nil
		}
	case SendCommandEvent:
		{
			if !h.JWT.HasPermission(PermissionSendCommand) {
				return nil
			}

			if h.server.State == server.ProcessOfflineState {
				return nil
			}

			return h.server.Environment.SendCommand(strings.Join(m.Args, ""))
		}
	}

	return nil
}
