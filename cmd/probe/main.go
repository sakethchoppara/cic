// probe is the Week 1 Docker SDK check: list containers, start one idle
// container, then remove it. Delete once internal/runner exists.
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

const image = "python:3.12-slim"

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cli, err := client.New(client.FromEnv)
	if err != nil {
		log.Fatal(err)
	}
	defer cli.Close()

	list, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d containers on this host\n", len(list.Items))
	for _, c := range list.Items {
		fmt.Printf("  %.12s  %-25s %s\n", c.ID, c.Image, c.State)
	}

	// Pull is left to the user (`docker pull python:3.12-slim`) to keep the probe small.
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:  image,
			Cmd:    []string{"sleep", "infinity"},
			User:   "nobody",
			Labels: map[string]string{"cic": "probe"},
		},
		HostConfig: &container.HostConfig{NetworkMode: "none"},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		// Fresh context: still clean up if ctx already expired.
		_, err := cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
		if err != nil {
			log.Printf("remove %.12s: %v", created.ID, err)
		}
		fmt.Printf("removed %.12s\n", created.ID)
	}()

	start := time.Now()
	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		log.Print(err)
		return
	}
	fmt.Printf("started idle container %.12s in %v (cold start, excluding create)\n", created.ID, time.Since(start))
}
