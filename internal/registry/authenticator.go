package registry

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/nibzard/ohya/internal/hubsync"
)

type Authenticator struct {
	DB           *DB
	PollInterval time.Duration
}

func (a *Authenticator) Authenticate(ctx context.Context, conn net.Conn) (string, error) {
	hello, pair, err := hubsync.ReadAuthFrame(conn)
	if err != nil {
		return "", err
	}
	if hello != nil {
		deviceID, err := a.DB.Authenticate(ctx, hello.Credential)
		if err != nil {
			return "", err
		}
		return deviceID, nil
	}
	credential, err := a.DB.Consume(ctx, pair.Invitation, pair.DeviceID)
	if err != nil {
		return "", err
	}
	if err := hubsync.WriteCredentialReply(conn, hubsync.CredentialReply{
		DeviceID:   credential.DeviceID,
		Credential: credential.Secret,
	}); err != nil {
		return "", fmt.Errorf("registry: deliver credential: %w", err)
	}
	return credential.DeviceID, nil
}

func (a *Authenticator) Authorize(ctx context.Context, deviceID string) error {
	return a.DB.Authorize(ctx, deviceID)
}

func (a *Authenticator) WatchRevocations(ctx context.Context, fn func(deviceID string)) error {
	return a.DB.WatchRevocations(ctx, a.PollInterval, fn)
}
