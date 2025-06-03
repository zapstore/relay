package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

type ZapstoreConfig struct {
	Repository string   `yaml:"repository"`
	Assets     []string `yaml:"assets"`
}

func publishApp(repository *url.URL) bool {
	data, err := yaml.Marshal(&ZapstoreConfig{
		Repository: strings.Trim(repository.String(), "/"),
		Assets:     []string{".*.apk"},
	})
	if err != nil {
		log.Println("Error marshaling YAML:", err)
		return false
	}

	name, err := getYamlFileName(repository)
	if err != nil {
		log.Println("Error parsing the name:", err)
		return false
	}

	fpath := path.Join(config.WorkingDirectory, name)
	err = writeFile(fpath, data)
	if err != nil {
		log.Println("Error writing YAML file:", err)
		return false
	}

	log.Printf("About to index %s", fpath)
	out, code, err := runCLI("zapstore-cli", "--no-auto-update", "publish", "-c", fpath, "--daemon-mode")
	log.Printf("Got result %s", out)

	if err != nil {
		log.Println("Error running cli:", err)
		return false
	}

	if code == 1 {
		if err := db.Savelog(context.Background(), out); err != nil {
			log.Fatalln("Error writing log to database:", err)
		}

		return false
	}

	if code == 0 {
		log.Printf("New software indexed: %s\n", repository)
	}

	return true
}

func runCLI(name string, args ...string) (string, int, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = os.Environ()

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

func getGithubURL(s string) (*url.URL, error) {
	parsedUrl, err := url.Parse(s)
	segments := strings.Split(strings.Trim(parsedUrl.Path, "/"), "/")

	if len(segments) != 2 {
		return nil, fmt.Errorf("URL does not contain exactly user and repo %s", segments)
	}

	if err != nil {
		return nil, err
	}
	return parsedUrl, nil
}

func getYamlFileName(githubUrl *url.URL) (string, error) {
	segments := strings.Split(strings.Trim(githubUrl.Path, "/"), "/")
	user := segments[0]
	repo := segments[1]

	repo = strings.TrimSuffix(repo, ".git")

	filename := fmt.Sprintf("%s-%s.yaml", user, repo)
	return filename, nil
}
