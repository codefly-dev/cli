package deployment

import (
	"context"
)

type Github struct {
}

func (github *Github) CreateRepository(ctx context.Context, name string) error {
	//
	//ts := oauth2.StaticTokenSource(
	//	&oauth2.Token{AccessToken: "<YOUR_GITHUB_TOKEN>"},
	//)
	//tc := oauth2.NewClient(ctx, ts)
	//
	//client := github.NewClient(tc)
	//
	//// create a new private repository
	//repo := &github.Repository{
	//	Name:    github.String("my-new-repository"),
	//	Private: github.Bool(true),
	//}
	//
	//repository, _, err := client.Repositories.Create(ctx, "", repo)
	//if err != nil {
	//	fmt.Println(err)
	//	return
	//}
	//
	//fmt.Println(*repository.CloneURL)
	return nil
}
