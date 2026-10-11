package gitops

import (
	"context"
	"fmt"
	"strings"
)

// bindRepository binds local planning reads and every later write to the
// directory at the declared path, and preserves the inspection's binding
// when preparation is allowed to advertise its snapshot.
func (p *deliveryPublication) bindRepository(ctx context.Context, declared string, selfPromotes bool, expected []*LocalRepositoryIdentity) (string, *LocalRepositoryIdentity, error) {
	if !selfPromotes {
		return declared, nil, nil
	}
	bound, err := bindLocalRepository(ctx, declared)
	if err != nil {
		return "", nil, err
	}
	if len(expected) != 0 && expected[0] != nil && *expected[0] != *bound {
		return "", nil, fmt.Errorf("the local repository changed after inspection; plan again")
	}
	p.localRepository, p.repositoryURL = bound, declared
	return "file://" + bound.Path, bound, nil
}

// git verifies the binding around every access, including each individual
// write. Git uses its working directory as the repository, so it does not
// reopen the absolute directory name for each file after starting.
func (bound *LocalRepositoryIdentity) git(ctx context.Context, declared string, args ...string) (string, error) {
	if err := bound.verify(ctx, declared); err != nil {
		return "", err
	}
	out, err := isolatedGit(ctx, bound.Path, args...)
	if verifyErr := bound.verify(ctx, declared); verifyErr != nil {
		return "", verifyErr
	}
	return out, err
}

func (bound *LocalRepositoryIdentity) publishRef(ctx context.Context, declared, checkout, commit, ref string) error {
	old, err := bound.git(ctx, declared, "show-ref", "--verify", "--hash", ref)
	if err != nil {
		// Distinguish an absent ref from a changed repository or broken Git.
		if _, err = bound.git(ctx, declared, "rev-parse", "--git-dir"); err != nil {
			return err
		}
		old = strings.Repeat("0", len(commit))
	}
	return bound.advance(ctx, declared, checkout, commit, ref, old)
}

func (bound *LocalRepositoryIdentity) advance(ctx context.Context, declared, checkout, commit, ref, old string) error {
	if _, err := bound.git(ctx, declared, "fetch", "--no-tags", "--quiet", checkout, commit); err != nil {
		return fmt.Errorf("bring the signed commit into the bound repository: %w", err)
	}
	if strings.Trim(old, "0") != "" {
		if _, err := bound.git(ctx, declared, "merge-base", "--is-ancestor", old, commit); err != nil {
			return fmt.Errorf("local ref %s only fast-forwards from %s: %w", ref, old, err)
		}
	}
	if _, err := bound.git(ctx, declared, "update-ref", "-m", "codefly: local publication", ref, commit, old); err != nil {
		return fmt.Errorf("fast-forward %s (the base moved since planning or the ref is locked): %w", ref, err)
	}
	moved, err := bound.git(ctx, declared, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil || moved != commit {
		return fmt.Errorf("local ref %s reads %q, expected %s (%v)", ref, moved, commit, err)
	}
	return nil
}

func publicationRemoteRef(ctx context.Context, prepared *preparedRepository, ref string) (string, error) {
	if prepared.plan.SelfPromotes {
		commit, err := prepared.plan.LocalRepository.git(ctx, prepared.plan.Repository, "rev-parse", "--verify", ref+"^{commit}")
		return commit + "\t" + ref, err
	}
	return gitCommand(ctx, prepared.dir, "ls-remote", "--exit-code", "--refs", "origin", ref)
}
