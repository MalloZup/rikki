package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/jrallison/go-workers"
)

// Analyzer is a job that provides feedback on specific issues in the code.
// The job receives the uuid of a submission, calls the exercism API to get
// the code, submits the code to analysseur for static analysis, and then,
// based on the results, chooses a response to submit as a comment from rikki-
// back to the conversation on exercism.
type Analyzer struct {
	exercism       *Exercism
	analysseurHost string
	comments       map[string][]byte
}

// NewAnalyzer configures an analyzer job to talk to the exercism and analysseur APIs.
func NewAnalyzer(exercism *Exercism, analysseur, dir string) (*Analyzer, error) {
	dir = filepath.Join(dir, "analyzer", "ruby")

	comments := make(map[string][]byte)

	fn := func(path string, info os.FileInfo, err error) error {
		if info.IsDir() {
			return nil
		}
		b, err := read(path)
		if err != nil {
			return err
		}
		r := strings.NewReplacer(dir, "", ".md", "")
		key := r.Replace(path)
		key = strings.TrimLeft(key, "/")

		comments[key] = b

		return nil
	}

	if err := filepath.Walk(dir, fn); err != nil {
		return nil, err
	}

	return &Analyzer{
		exercism:       exercism,
		analysseurHost: analysseur,
		comments:       comments,
	}, nil
}

type analysisResult struct {
	Type string   `json:"type"`
	Keys []string `json:"keys"`
}
type analysisPayload struct {
	Results []analysisResult `json:"results"`
	Error   string           `json:"error"`
}

func (analyzer *Analyzer) process(msg *workers.Msg) {
	uuid, err := msg.Args().GetIndex(0).String()
	if err != nil {
		lgr.Printf("unable to determine submission key - %s\n", err)
		return
	}

	solution, err := analyzer.exercism.FetchSolution(uuid)
	if err != nil {
		lgr.Printf("%s\n", err)
		return
	}

	if solution.TrackID != "ruby" {
		lgr.Printf("skipping - rikki- doesn't support %s\n", solution.TrackID)
		return
	}

	var sources []string
	for _, source := range solution.Files {
		sources = append(sources, source)
	}

	// Step 2: submit code to analysseur
	url := fmt.Sprintf("%s/analyze/%s", analyzer.analysseurHost, solution.TrackID)
	codeBody := struct {
		Code string `json:"code"`
	}{
		strings.Join(sources, "\n"),
	}
	codeBodyJSON, err := json.Marshal(codeBody)
	if err != nil {
		lgr.Printf("%s - %s\n", uuid, err)
		return
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(codeBodyJSON))
	if err != nil {
		lgr.Printf("%s - cannot prepare request to %s - %s\n", uuid, url, err)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		lgr.Printf("%s - request to %s failed - %s\n", uuid, url, err)
		return
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		lgr.Printf("%s - failed to fetch submission - %s\n", uuid, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		lgr.Printf("%s - %s responded with status %d - %s\n", uuid, url, resp.StatusCode, string(body))
		return
	}

	var ap analysisPayload
	err = json.Unmarshal(body, &ap)
	if err != nil {
		lgr.Printf("%s - %s\n", uuid, err)
		return
	}

	if ap.Error != "" {
		lgr.Printf("analysis api is complaining about %s - %s\n", uuid, ap.Error)
		return
	}

	if len(ap.Results) == 0 {
		// no feedback, bailing
		return
	}

	var smells []string
	sanity := log.New(os.Stdout, "SANITY: ", log.Ldate|log.Ltime|log.Lshortfile)
	for _, result := range ap.Results {
		for _, key := range result.Keys {
			sanity.Printf("%s : %s - %s\n", uuid, result.Type, key)

			smells = append(smells, filepath.Join(result.Type, key))
		}
	}

	// shuffle code smells
	for i := range smells {
		j := rand.Intn(i + 1)
		smells[i], smells[j] = smells[j], smells[i]
	}

	// return the first available comment
	var comment []byte
	for _, smell := range smells {
		b := analyzer.comments[smell]

		if len(b) > 0 {
			comment = b
			break
		}
	}

	if len(comment) == 0 {
		return
	}

	// Step 3: submit random comment to exercism.io api
	if err := analyzer.exercism.SubmitComment(comment, uuid); err != nil {
		lgr.Printf("%s\n", err)
	}
}
