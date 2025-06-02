package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os/exec"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

type ZapstoreConfig struct {
	Repository string   `yaml:"repository"`
	Assets     []string `yaml:"assets"`
}

func publishApp(repository string) {
	data, err := yaml.Marshal(&ZapstoreConfig{
		Repository: repository,
		Assets:     []string{".*.apk"},
	})
	if err != nil {
		log.Println("Error marshaling YAML:", err)
		return
	}

	name, err := githubURLToYAML(repository)
	if err != nil {
		log.Println("Error parsing the name:", err)
		return
	}

	fpath := path.Join(config.WorkingDirectory, name)
	err = writeFile(fpath, data)
	if err != nil {
		log.Println("Error writing YAML file:", err)
		return
	}

	out, code, err := runCLI("zapstore-cli", "--no-auto-update", "publish", "-c", fpath)
	if err != nil {
		log.Println("Error running cli:", err)
		return
	}

	if code == 1 {
		if err := db.Savelog(context.Background(), out); err != nil {
			log.Fatalln("Error writing log to database:", err)
		}
	}

	if code == 0 {
		log.Printf("New software indexed: %s\n", repository)
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

func githubURLToYAML(githubUrl string) (string, error) {
	parsedURL, err := url.Parse(githubUrl)
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
