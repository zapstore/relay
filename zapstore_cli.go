package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"gopkg.in/yaml.v3"
)

type PubApp struct {
	Repository string `yaml:"repository"`
}

func PublishApp(r string) {
	data, err := yaml.Marshal(&PubApp{
		Repository: r,
	})
	if err != nil {
		log.Println("Error marshaling YAML:", err)
		return
	}

	name, err := githubURLToYAML(r)
	if err != nil {
		log.Println("Error parsing the name:", err)
		return
	}

	err = os.WriteFile(name, data, 0o644)
	if err != nil {
		log.Println("Error writing YAML file:", err)
		return
	}

	out, code, err := runCLI("zapstore-cli", "publish", "-c", name)
	if err != nil {
		log.Println("Error running cli:", err)
		return
	}

	if code == 1 {
		if err := db.Savelog(context.Background(), out); err != nil {
			log.Fatalln("Error writing log to database:", err)
		}
	}

	if code == 2 {
		// add nothing to index in database
	}

	if code == 0 {
		// successful.
	}
}

func runCLI(name string, args ...string) (string, int, error) {
	cmd := exec.Command(name, args...)

	outBytes, err := cmd.CombinedOutput()
	output := string(outBytes)

	exitCode := 0

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return output, -1, err
		}
	}

	return output, exitCode, nil
}

func githubURLToYAML(gurl string) (string, error) {
	parsedURL, err := url.Parse(gurl)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %v", err)
	}

	segments := strings.Split(strings.Trim(parsedURL.Path, "/"), "/")
	if len(segments) < 2 {
		return "", fmt.Errorf("URL does not contain user and repo")
	}

	user := segments[0]
	repo := segments[1]

	repo = strings.TrimSuffix(repo, ".git")

	filename := fmt.Sprintf("%s-%s.yaml", user, repo)
	return filename, nil
}
