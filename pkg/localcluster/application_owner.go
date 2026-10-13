package localcluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/codefly-dev/cli/pkg/gitops"
)

// appliedApplicationSets reads the server-assigned identities from apply's
// response, rather than trusting an Application's claim about its owner.
func appliedApplicationSets(response string) ([]gitops.ApplicationSetIdentity, error) {
	type object struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
			UID       string `json:"uid"`
		} `json:"metadata"`
		Items []object `json:"items"`
	}
	decoder := json.NewDecoder(strings.NewReader(response))
	var objects []object
	for {
		var item object
		if err := decoder.Decode(&item); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("read applied bootstrap identities: %w", err)
		}
		if item.Kind == "List" {
			objects = append(objects, item.Items...)
		} else {
			objects = append(objects, item)
		}
	}
	var sets []gitops.ApplicationSetIdentity
	for _, item := range objects {
		if item.Kind == "ApplicationSet" {
			sets = append(sets, gitops.ApplicationSetIdentity{APIVersion: item.APIVersion, Kind: item.Kind,
				Name: item.Metadata.Name, Namespace: item.Metadata.Namespace, UID: item.Metadata.UID})
		}
	}
	return sets, nil
}

func bindApplicationOwners(jobs []gitops.DeliveryJob, sets []gitops.ApplicationSetIdentity) error {
	for index := range jobs {
		job := &jobs[index]
		want := &job.Application.Owner
		matches := 0
		for _, set := range sets {
			if set.APIVersion == want.APIVersion && set.Kind == want.Kind && set.Namespace == want.Namespace && set.Name == want.Name && set.UID != "" {
				want.UID = set.UID
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("delivery Application %s requires exactly one applied ApplicationSet %s/%s with a UID; got %d", job.Application.Name, want.Namespace, want.Name, matches)
		}
	}
	return nil
}
