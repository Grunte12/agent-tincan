package client

import "context"

// RelayUpgradeRequest asks a relay to install a release over its own binary.
type RelayUpgradeRequest struct {
	// Force installs the dist release even when it is not newer.
	Force bool `json:"force,omitempty"`
	// FromGitHub, when set, is the release tag (v0.8.0) the relay first
	// downloads from its release URL into its dist directory.
	FromGitHub string `json:"from_github,omitempty"`
}

// RelayUpgradeResult is a relay's reply to a self-upgrade.
type RelayUpgradeResult struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Restart string `json:"restart"` // "re-exec" or "exit"
}

// RelayUpgradePath is the admin route for a relay self-upgrade.
const RelayUpgradePath = "/v1/admin/relay/upgrade"

// RelayUpgrade asks the relay (admin only) to upgrade itself. The relay may
// download a release first, so the call gets the dist download timeout. It
// is never retried at a relocated address: the relay may be restarting.
func (r *Relay) RelayUpgrade(ctx context.Context, in RelayUpgradeRequest) (RelayUpgradeResult, error) {
	var out RelayUpgradeResult
	err := r.callOnce(ctx, r.withTimeout(DistDownloadTimeout), "POST", RelayUpgradePath, in, &out)
	return out, err
}
