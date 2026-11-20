// Package labels defines the container labels the webhook understands.
package labels

const (
	// Prefix is the namespace of every label used by the webhook.
	Prefix = "skvostik.redeploy."

	// ID enables a container for redeploys and sets its webhook id.
	ID = Prefix + "id"

	// GitHubRepository enables GitHub Actions OIDC authentication. The value
	// is either "owner/repo" or a numeric repository ID.
	GitHubRepository = Prefix + "github.repository"

	// GitHubRef optionally restricts OIDC tokens to a git ref (e.g. "refs/heads/main").
	GitHubRef = Prefix + "github.ref"

	// GitHubEnvironment optionally restricts OIDC tokens to a GitHub environment.
	GitHubEnvironment = Prefix + "github.environment"
)
