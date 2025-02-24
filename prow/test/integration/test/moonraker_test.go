/*
Copyright 2023 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package integration

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	prowjobv1 "k8s.io/test-infra/prow/apis/prowjobs/v1"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/git/v2"
	"k8s.io/test-infra/prow/moonraker"
	"k8s.io/test-infra/prow/test/integration/internal/fakegitserver"
)

type fakeConfigAgent struct {
	sync.Mutex
	c *config.Config
}

func (fca *fakeConfigAgent) Config() *config.Config {
	fca.Lock()
	defer fca.Unlock()
	return fca.c
}

// TestMoonrakerBurst tests Moonraker by flooding it with a burst of 100
// requests at once. Each request will have the same base SHA, but a different
// head SHA (pretending to be a unique Pull Request). This way, the requests
// will avoid getting coalesced into the same LRU cache entry. This in turn will
// force Moonraker to fetch the same base SHA in parallel while constructing the
// ProwYAML value. We expect Moonraker to return successfully for all 100
// requests.
//
// The hard part here is constructing the 100 different head SHA values. We need
// to create 100 different PRs, each with its own unique head SHA. We do this
// dynamically with our own Git v2 client (this means we will write into our
// local filesystem). We send a shell script over to fakegitserver (FGS) to
// create the 100 different commit SHAs, but all using the same base SHA (this
// is how FGS lets clients seed test repos). We then clone the repo from FGS to
// our local disk to read the SHA values for these 100 fake PRs. Now that we
// know the repo location, the base SHA, and the head SHAs, we can construct the
// GetProwYAML() queries to Moonraker. We then just check that the return values
// for all 100 requests are the same (because the 100 PRs will not modify the
// inrepoconfig files).
//
// The reason why we do this as an integration test instead of a unit test is
// because we want to go over the network for fetching the SHA object from FGS,
// as opposed to "fetching" locally from disk.
func TestMoonrakerBurst(t *testing.T) {
	const createRepoWithPRs = `
echo hello > README.txt
git add README.txt
git commit -m "commit 1"

# Create fake PRs. These are "Gerrit" style refs under refs/changes/*.
for num in $(seq 1 100); do
	git checkout -d master
	echo "${num}" > "${num}"
	git add "${num}"
	git commit -m "PR${num}"
	git update-ref "refs/changes/00/123/${num}" HEAD
done

git checkout master

echo this-is-from-repo%s > README.txt
git add README.txt
git commit -m "uniquify"

mkdir .prow
cat <<EOF >.prow/presubmits.yaml
presubmits:
  - name: my-presubmit
    always_run: false
    decorate: true
    spec:
      containers:
      - image: localhost:5001/alpine
        command:
        - sh
        args:
        - -c
        - |
          set -eu
          echo "hello from my-presubmit"
          cat README.txt
EOF

git add .prow/presubmits.yaml
git commit -m "add inrepoconfig for my-presubmit"
`

	repoSetup := fakegitserver.RepoSetup{
		Name:      "moonraker-burst",
		Script:    createRepoWithPRs,
		Overwrite: true,
	}

	// Set up repos on FGS for just this test case.
	fgsClient := fakegitserver.NewClient("http://localhost/fakegitserver", 5*time.Second)
	err := fgsClient.SetupRepo(repoSetup)
	if err != nil {
		t.Fatalf("FGS repo setup failed: %v", err)
	}

	// Clone the repo out of FGS to determine the base SHA of master branch for
	// repo-moonraker-stres-test, as well as the 100 different PR head SHAs.
	cacheDirBase := "/var/tmp"

	trueVal := true
	var gitClientFactoryOpt = func(o *git.ClientFactoryOpts) {
		o.UseInsecureHTTP = &trueVal
		o.Host = "localhost"
		o.CacheDirBase = &cacheDirBase
	}

	gc, err := git.NewClientFactory(gitClientFactoryOpt)
	if err != nil {
		t.Fatal(err)
	}

	var repoOpts = git.RepoOpts{
		ShareObjectsWithSourceRepo: true,
		NoFetchTags:                true,
	}

	// repoClient points to the *secondary* clone. Still, because we share all
	// objects with the primary, it effectively behaves like the primary with
	// all of the (mirrored) refs in it.
	repoClient, err := gc.ClientForWithRepoOpts("fakegitserver/repo", repoSetup.Name, repoOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer repoClient.Clean()

	baseSHA, err := repoClient.RevParse("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	baseSHA = strings.TrimSuffix(baseSHA, "\n")

	// We fetch all refs/changes/* into the secondary clone. Although we share
	// the underlying SHA objects (ShareObjectsWithSourceRepo), we don't know
	// what these SHAs look like yet. However, we do know that the
	// refs/changes/* files act as pointers to the SHAs, so fetch these refs.
	repoClient.Fetch("refs/changes/*:refs/changes/*")
	refs := []string{}
	for i := 1; i <= 100; i++ {
		ref := fmt.Sprintf("refs/changes/00/123/%d", i)
		refs = append(refs, ref)
	}
	refsToShas, err := repoClient.RevParseN(refs)
	if err != nil {
		t.Fatal(err)
	}

	headSHAs := []string{}
	for _, headSHA := range refsToShas {
		headSHAs = append(headSHAs, headSHA)
	}

	// Set up client to talk to Moonraker inside the KIND cluster. The moonraker
	// address here uses localhost, because we're initiating the request from
	// outside the KIND cluster (this file you are reading is executed outside
	// the cluster).
	fca := &fakeConfigAgent{
		c: &config.Config{
			ProwConfig: config.ProwConfig{
				Moonraker: config.Moonraker{
					ClientTimeout: &metav1.Duration{Duration: 5 * time.Second},
				},
			},
		},
	}
	moonrakerClient, err := moonraker.NewClient("http://localhost/moonraker", fca)
	if err != nil {
		t.Fatal(err)
	}

	var want = config.Presubmit{
		JobBase: config.JobBase{
			Name: "my-presubmit",
			UtilityConfig: config.UtilityConfig{
				Decorate: &trueVal,
			},
			Spec: &v1.PodSpec{
				Containers: []v1.Container{
					{
						Image:   "localhost:5001/alpine",
						Command: []string{"sh"},
						Args: []string{
							"-c",
							`set -eu
echo "hello from my-presubmit"
cat README.txt
`,
						},
					},
				},
			},
		},
	}

	// Collect errors from the workers. This is because otherwise we get a
	// "call to (*T).Fatal from a non-test goroutine" error.
	errs := make(chan error, 1)

	var sendGetProwYAMLRequest = func(t *testing.T, headSHA string) {
		got, err := moonrakerClient.GetProwYAML(&prowjobv1.Refs{
			// org is the URL of the "org" for the repo, as seen from inside the
			// KIND cluster (because moonraker is running inside the KIND
			// cluster). This is why we use the "fakegitserver.default" domain.
			Org:     "https://fakegitserver.default/repo",
			Repo:    "moonraker-burst",
			BaseSHA: baseSHA,
			Pulls:   []prowjobv1.Pull{{SHA: headSHA}},
		})
		if err != nil {
			errs <- err
			return
		}

		gotPresubmit := got.Presubmits[0]
		// Clear out parts of the response that we don't care about.
		gotPresubmit.Trigger = ""
		gotPresubmit.RerunCommand = ""
		gotPresubmit.Reporter = config.Reporter{}
		gotPresubmit.Brancher = config.Brancher{}
		gotPresubmit.JobBase.Agent = ""
		gotPresubmit.JobBase.Cluster = ""
		gotPresubmit.JobBase.Namespace = nil
		gotPresubmit.JobBase.ProwJobDefault = nil
		gotPresubmit.UtilityConfig.DecorationConfig = nil

		// Check that the gotPresubmit is what we expect (what we created in the
		// createRepoWithPRs bit at the beginning of this test function above).
		if diff := cmp.Diff(gotPresubmit, want, cmpopts.IgnoreUnexported(
			config.Presubmit{},
			config.Brancher{},
			config.RegexpChangeMatcher{})); diff != "" {
			errs <- fmt.Errorf("unexpected moonraker response: %s", diff)
		} else {
			errs <- nil
		}
	}

	// Now create the burst of 100 requests.
	for i := 1; i <= 100; i++ {
		go sendGetProwYAMLRequest(t, headSHAs[i-1])
	}

	// Check that all 100 requests finished successfully.
	for i := 1; i <= 100; i++ {
		err := <-errs
		if err != nil {
			t.Fatal(err)
		}
	}
}
