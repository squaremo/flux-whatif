package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

// This represents a "what if" scenario in which a git repo has a
// branch HEAD updated. It's called "merge" because the usual form of
// this scenario is "what if I merge this branch into main?".
type mergeScenario struct {
	url          string
	targetBranch string
	newRef       string
}

func (s mergeScenario) Description() string {
	return fmt.Sprintf("What if git repo %q branch %q HEAD is %q", s.url, s.targetBranch, s.newRef)
}

type mergeopts struct {
	*globalopts
	scenario mergeScenario
}

func (mo *mergeopts) addFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&mo.scenario.url, "url", "", "the URL of the git repository")
	cobra.MarkFlagRequired(cmd.Flags(), "url")
	cmd.Flags().StringVar(&mo.scenario.targetBranch, "branch", "main", "the branch to simulate being updated")
	cobra.MarkFlagRequired(cmd.Flags(), "branch")
	cmd.Flags().StringVar(&mo.scenario.newRef, "ref", "", "the ref to simulate being head of the branch; e.g., refs/heads/topic")
	cobra.MarkFlagRequired(cmd.Flags(), "ref")
}

func (mo *mergeopts) runE(cmd *cobra.Command, args []string) error {
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

func (s mergeScenario) findAffectedSources(ctx context.Context, k8sClient client.Client) (sourceMap, error) {
	return findRepos(ctx, k8sClient, s.url, func(repo *sourcev1.GitRepository) bool {
		branch := "master" // the default in Flux GitRepository; TODO find a const for this
		if ref := repo.Spec.Reference; ref != nil {
			switch {
			case strings.HasPrefix(ref.Name, "refs/heads/"):
				// Name takes precedence over Tag, Branch and SemVer
				branch = strings.TrimPrefix(ref.Name, "refs/heads/")
			case ref.Tag != "" ||
				ref.Commit != "" ||
				ref.SemVer != "":
				// none of those would be affected by a branch
				// head changing
				log.V(DEBUG).Info("GitRepository does not track target branch; skipping", "name", client.ObjectKeyFromObject(repo), "branch", s.targetBranch)
				return false
			case ref.Branch != "":
				branch = ref.Branch
			}
			if branch == s.targetBranch {
				nsn := client.ObjectKeyFromObject(repo)
				log.V(INFO).Info("including GitRepository", "name", nsn)
				repo.Spec.Reference = &sourcev1.GitRepositoryRef{
					Name: s.newRef,
				}
				return true
			} else {
				log.V(DEBUG).Info("GitRepository does not track target branch; skipping", "name", client.ObjectKeyFromObject(repo), "branch", s.targetBranch)
			}
		}
		return false
	})
}
