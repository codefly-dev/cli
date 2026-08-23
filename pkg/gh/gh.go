package gh

import "context"

// Archived reports whether owner/repo is an archived GitHub repository.
// A lookup failure (missing repo, rate limit, no auth, client construction)
// returns false so callers only skip a repo on a *confirmed* archived flag and
// otherwise degrade to their normal behavior rather than silently hiding live
// repos.
func Archived(ctx context.Context, owner, repo string) bool {
	if owner == "" || repo == "" {
		return false
	}
	client, err := NewClient()
	if err != nil || client == nil {
		return false
	}
	r, _, err := client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return false
	}
	return r.GetArchived()
}
