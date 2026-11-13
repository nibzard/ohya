// The registry-backed authenticator of the sync endpoint: it speaks the
// pairing and hello frames of internal/hubsync and backs them with the
// credential registry, implementing hubsync.Authenticator and
// hubsync.SessionAuthorizer for the Auth.tla authorization rules.

package registry

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/nibzard/ohya/internal/hubsync"
)

// Authenticator authenticates sync connections through the credential
// registry. The first frame of a connection is a pairing request (a stranger
// presenting a one-time invitation) or a hello (a paired device presenting
// its credential); a successful pairing continues as an authorized session of
// the newly admitted device. Every request frame is re-checked through
// Authorize, and revocations reach the endpoint through WatchRevocations.
//
// A Tailcat WhoIs-based authenticator (transport-level identity instead of a
// presented credential) can implement the same interfaces; the registry
// still decides admission and revocation.
type Authenticator struct {
	// DB is the hub's device credential registry.
	DB *DB
	// PollInterval bounds how quickly the endpoint learns of a revocation
	// committed by another process. Values <= 0 select DefaultPollInterval.
	PollInterval time.Duration
}

// Authenticate reads the connection's first frame and establishes the peer's
// device identity. On a pairing request it consumes the invitation — one
// atomic registry transaction — and delivers the minted credential before
// the session starts; on a hello it maps the credential to its active
// device. A refusal is an error; the endpoint reports it as CodeAuth.
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

// Authorize applies the per-request registry check: the device must be
// currently active. Revocation is final, so this is the check every request
// races and can lose.
func (a *Authenticator) Authorize(ctx context.Context, deviceID string) error {
	return a.DB.Authorize(ctx, deviceID)
}

// WatchRevocations reports each newly revoked device to the endpoint, which
// closes its connections and cancels its in-flight requests.
func (a *Authenticator) WatchRevocations(ctx context.Context, fn func(deviceID string)) error {
	return a.DB.WatchRevocations(ctx, a.PollInterval, fn)
}
