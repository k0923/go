package sqlite

import (
	"encoding/json"
	"time"

	"github.com/k0923/go/tflow"
)

func jsonIdentity(c tflow.Command) ([]byte, error) {
	identity := struct {
		Kind                                     tflow.CommandKind
		MainID, Execution, EventKey, ExternalRef string
		Main                                     *tflow.MainProgress
	}{Kind: c.Kind, MainID: c.MainID, Execution: c.Sub.Execution(c.MainID).Key(), EventKey: c.EventKey}
	if c.Kind == tflow.CancelSub {
		identity.ExternalRef = c.Sub.ExternalRef
	}
	if c.Kind == tflow.MainTerminated {
		progress := c.Main.MainProgress
		// Match the protocol identity, excluding incidental timestamp fields.
		progress.CreatedAt, progress.UpdatedAt = time.Time{}, time.Time{}
		identity.Main = &progress
	}
	return json.Marshal(identity)
}
