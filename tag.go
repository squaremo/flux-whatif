package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/fluxcd/pkg/git"
	"github.com/fluxcd/pkg/version"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

// This represents a "what if" scenario in which a git repo has a
// branch HEAD updated. It's called "merge" because the usual form of
// this scenario is "what if I merge this branch into main?".
type tagScenario struct {
	url    string
	newTag string
	ref    string
}

func (s tagScenario) Description() string {
	return fmt.Sprintf("What if git repo %q ref %q is tagged %q", s.url, s.ref, s.newTag)
}

type tagopts struct {
	*globalopts
	scenario tagScenario
}

func (mo *tagopts) addFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&mo.scenario.url, "url", "", "the URL of the git repository")
	cobra.MarkFlagRequired(cmd.Flags(), "url")
	cmd.Flags().StringVar(&mo.scenario.ref, "ref", "refs/heads/main", "the ref (exact commit or e.g., refs/heads/main)")
	cobra.MarkFlagRequired(cmd.Flags(), "ref")
	cmd.Flags().StringVar(&mo.scenario.newTag, "tag", "", "the new tag")
	cobra.MarkFlagRequired(cmd.Flags(), "tag")
}

func (mo *tagopts) runE(cmd *cobra.Command, args []string) error {
	cfg, err := config.GetConfig()
	if err != nil {
		return err
	}
	k8sClient, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	//   Before starting, make a working space
	tmpRoot, err := os.MkdirTemp("", "flux-whatif-")
	if err != nil {
		return err
	}
	log.V(INFO).Info("temporary working directory created", "path", tmpRoot, "keep", mo.keepTmp)
	if !mo.keepTmp {
		defer os.RemoveAll(tmpRoot)
	}

	ctx := context.Background()

	scenario := mo.scenario
	fmt.Fprintln(os.Stderr, scenario.Description())
	return simulate(ctx, tmpRoot, scenario, k8sClient)
}

func (s tagScenario) findAffectedSources(ctx context.Context, k8sClient client.Client) (sourceMap, error) {
	// List all the git repos, and find those with the particular repo
	// URL, and following the branch in question. Mutate each so they
	// point to the ref of the scenario.
	var gitrepos sourcev1.GitRepositoryList
	if err := k8sClient.List(ctx, &gitrepos, &client.ListOptions{}); err != nil { // TODO namespace?
		return nil, err
	}

	// keep a map, so we can look them up when finding Kustomizations that need to be applied.
	ver, err := version.ParseVersion(s.newTag)
	validVersion := err == nil

	useRef := func(r *sourcev1.GitRepository) {
		r.Spec.Reference = &sourcev1.GitRepositoryRef{
			Name: s.ref,
		}
	}

	return findRepos(ctx, k8sClient, s.url, func(repo *sourcev1.GitRepository) bool {
		reponame := client.ObjectKeyFromObject(repo)
		if ref := repo.Spec.Reference; ref != nil {
			switch {
			case strings.HasPrefix(ref.Name, "refs/tags/"):
				// Name takes precedence over Tag, Branch and SemVer
				tag := strings.TrimPrefix(ref.Name, "refs/tags/")
				if tag == s.newTag {
					log.V(INFO).Info("including GitRepository", "name", reponame)
					useRef(repo)
					return true
				}
			case ref.Tag != "":
				if ref.Tag == s.newTag {
					log.V(INFO).Info("including GitRepository", "name", reponame)
					useRef(repo)
					return true
				}
			case ref.SemVer != "":
				if !validVersion {
					log.V(DEBUG).Info("GitRepository matching semver, but given tag is not a valid version; skipping", "name", reponame)
					return false
				}
				// it has to match, but it also has to be more
				// recent that what has been seen already (or what's in the repo?)
				constraint, err := semver.NewConstraint(ref.SemVer)
				if err != nil {
					log.V(WARNING).Info("unable to parse semver constraint in repo", "name", reponame, "error", err)
					return false
				}
				if constraint.Check(ver) {
					if repo.Status.Artifact != nil && repo.Status.Artifact.Revision != "" {
						rev := repo.Status.Artifact.Revision
						tag := git.ExtractNamedPointerFromRevision(rev)
						repover, err := version.ParseVersion(tag)
						if err != nil {
							log.V(WARNING).Info("unable to parse what should be a tag", "name", reponame, "error", err, "ref", tag)
							return false
						}
						if ver.Equal(repover) || ver.LessThan(repover) {
							log.V(DEBUG).Info("GitRepository matching semver, but new tag <= observed tag; skipping", "name", reponame, "tag", tag, "new tag", s.newTag)
							return false
						}
					}
					log.V(INFO).Info("including repo", "name", reponame)
					useRef(repo)
					return true
				} else {
					log.V(DEBUG).Info("GitRepository semver does not admit new tag; skipping", "name", reponame, "new tag", s.newTag)
				}
			}
		}
		return false
	})
}
