/*
Quick and simple monitoring system

Usage:
 jsonmon /etc/jsonmon/config.yml
 jsonmon -v # Prints version to stdout and exits

Environment:
 HOST
  - defaults to localhost
  - the JSON API network interface
 PORT
  - defaults to 3000
  - the JSON API port
 GOMAXPROCS
  - defaults to CPU number + 1
  - number of threads to start
*/
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"gopkg.in/yaml.v2"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// Application version.
const Version = "1.1"

// This one is for internal use.
type ver struct {
	App  string `json:"jsonmon"`
	Go   string `json:"runtime"`
	Os   string `json:"os"`
	Arch string `json:"arch"`
}

var version ver

// Check details.
type Check struct {
	Name   string      `json:"name,omitempty" yaml:"name"`
	Web    string      `json:"web,omitempty" yaml:"web"`
	Shell  string      `json:"shell,omitempty" yaml:"shell"`
	Match  string      `json:"-" yaml:"match"`
	Return int         `json:"-" yaml:"return"`
	Notify interface{} `json:"-" yaml:"notify"`
	Alert  interface{} `json:"-", yaml:"alert`
	Tries  int         `json:"-" yaml:"tries"`
	Repeat int         `json:"-" yaml:"repeat"`
	Failed bool        `json:"failed" yaml:"-"`
	Since  string      `json:"since,omitempty" yaml:"-"`
}

// Global checks list. Need to share it with workers and Web UI.
var checks []Check

// The main loop.
func main() {
	// Parse CLI args.
	usage := "Usage: " + path.Base(os.Args[0]) + " /etc/jsonmon/config.yml\n" +
		"Docs:  https://github.com/chillum/jsonmon/wiki"
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
	// -v for version.
	version.App = Version
	version.Go = runtime.Version()
	version.Os = runtime.GOOS
	version.Arch = runtime.GOARCH
	switch os.Args[1] {
	case "-h":
		fallthrough
	case "-help":
		fallthrough
	case "--help":
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(0)
	case "-v":
		fallthrough
	case "-version":
		fallthrough
	case "--version":
		json, _ := json.MarshalIndent(&version, "", "  ")
		fmt.Println(string(json))
		os.Exit(0)
	}
	// Tune concurrency.
	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(runtime.NumCPU() + 1)
	}
	// Read config file or exit with error.
	config, err := ioutil.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(3)
	}
	err = yaml.Unmarshal(config, &checks)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", config[0], err)
		os.Exit(3)
	}
	// Run checks.
	var wg sync.WaitGroup
	wg.Add(1)
	for i := range checks {
		go worker(&checks[i])
	}
	// Launch the JSON API.
	host := os.Getenv("HOST")
	port := os.Getenv("PORT")
	if host == "" {
		host = "localhost"
	}
	if port == "" {
		port = "3000"
	}
	http.HandleFunc("/version", getVersion)
	http.HandleFunc("/", getChecks)
	err = http.ListenAndServe(host+":"+port, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
	}
	// Wait forever.
	wg.Wait()
}

// Background worker.
func worker(check *Check) {
	for {
		if check.Repeat == 0 { // Set default timeout.
			check.Repeat = 60
		}
		if check.Tries == 0 { // Default to 1 attempt.
			check.Tries = 1
		}
		if check.Web != "" {
			web(check)
		}
		if check.Shell != "" {
			shell(check)
		}
		time.Sleep(time.Second * time.Duration(check.Repeat))
	}
}

// Shell worker.
func shell(check *Check) {
	// Set check's display name.
	var name string
	if check.Name != "" {
		name = check.Name
	} else {
		name = check.Shell
	}
	// Execute with shell in N attemps.
	var out []byte
	var err error
	for i := 0; i < check.Tries; i++ {
		out, err = exec.Command("/bin/sh", "-c", check.Shell).CombinedOutput()
		if err == nil {
			if check.Match != "" { // Match regexp.
				var regex *regexp.Regexp
				regex, err = regexp.Compile(check.Match)
				if err == nil && !regex.Match(out) {
					err = errors.New("ERROR: output did not match " + check.Match)
				}
			}
			break
		}
	}
	// Process results.
	if err == nil {
		if check.Failed {
			check.Failed = false
			check.Since = time.Now().Format(time.RFC3339)
			notify(check.Notify, "Fixed: "+name, nil)
			alert(check, &name, nil)
		}
	} else {
		if !check.Failed {
			check.Failed = true
			check.Since = time.Now().Format(time.RFC3339)
			msg := string(out) + err.Error()
			notify(check.Notify, "Failed: "+name, &msg)
			alert(check, &name, &msg)
		}
	}
}

// Web worker.
func web(check *Check) {
	// Set check's display name.
	var name string
	if check.Name != "" {
		name = check.Name
	} else {
		name = check.Web
	}
	if check.Return == 0 { // Successful HTTP return code is 200.
		check.Return = 200
	}
	// Get the URL in N attempts.
	var err error
	for i := 0; i < check.Tries; i++ {
		err = fetch(check.Web, check.Match, check.Return)
		if err == nil {
			break
		}
	}
	// Process results.
	if err == nil {
		if check.Failed {
			check.Failed = false
			check.Since = time.Now().Format(time.RFC3339)
			notify(check.Notify, "Fixed: "+name, nil)
			alert(check, &name, nil)

		}
	} else {
		if !check.Failed {
			check.Failed = true
			check.Since = time.Now().Format(time.RFC3339)
			msg := err.Error()
			notify(check.Notify, "Failed: "+name, &msg)
			alert(check, &name, &msg)
		}
	}
}

// The actual HTTP GET.
func fetch(url string, match string, code int) error {
	var err error
	var resp *http.Response
	resp, err = http.Get(url)
	if err == nil && resp.StatusCode != code {
		err = errors.New(url + " returned " + strconv.Itoa(resp.StatusCode))
	} else { // Match regexp.
		if resp != nil && match != "" {
			var regex *regexp.Regexp
			regex, err = regexp.Compile(match)
			if err == nil {
				var body []byte
				body, _ = ioutil.ReadAll(resp.Body)
				if !regex.Match(body) {
					err = errors.New(url + " output did not match " + match)
				}
			}
		}
	}
	if resp != nil {
		resp.Body.Close()
	}
	return err
}

// Logs and mail alerting.
func notify(mail interface{}, subject string, message *string) {
	// Log the alerts.
	if message == nil {
		fmt.Println(subject)
	} else {
		fmt.Println(subject + "\n" + *message)
	}
	// Mail the alerts.
	if mail != nil {
		// Make the message.
		var rcpt string
		var ok bool
		if rcpt, ok = mail.(string); !ok {
			for i, v := range mail.([]interface{}) {
				if i != 0 {
					rcpt += ", "
				}
				rcpt += v.(string)
			}
		}
		msg := "To: " + rcpt + "\nSubject: " + subject + "\n\n"
		if message != nil {
			msg += *message
		}
		msg += "\n.\n"
		// And send it.
		sendmail := exec.Command("/usr/sbin/sendmail", "-t")
		stdin, _ := sendmail.StdinPipe()
		err := sendmail.Start()
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
		}
		io.WriteString(stdin, msg)
		sendmail.Wait()
	}
}

// Executes callback. Passes args: true/false, check's name, message.
func alert(check *Check, name *string, msg *string) {
	if check.Alert != nil {
		plugin, ok := check.Alert.(string)
		if ok { // check.Alert is a string.
			out, err := exec.Command(plugin, strconv.FormatBool(check.Failed), *name, *msg).CombinedOutput()
			if err != nil {
				fmt.Fprintln(os.Stderr, "ERROR:", string(out)+err.Error())
			}
		} else { // check.Alert is a list.
			for _, i := range check.Alert.([]interface{}) {
				out, err := exec.Command(i.(string), strconv.FormatBool(check.Failed), *name, *msg).CombinedOutput()
				if err != nil {
					fmt.Fprintln(os.Stderr, "ERROR:", string(out)+err.Error())
				}
			}
		}
	}
}

// Display checks' details.
func getChecks(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" { // Serve for root page only, 404 otherwise.
		w.Header().Set("Server", "jsonmon")
		http.NotFound(w, r)
		return
	}
	displayJSON(w, &checks)
}

// Display application version.
func getVersion(w http.ResponseWriter, r *http.Request) {
	displayJSON(w, &version)
}

// Output JSON.
func displayJSON(w http.ResponseWriter, data interface{}) {
	json, _ := json.MarshalIndent(&data, "", "  ")
	w.Header().Set("Server", "jsonmon")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write(json)
	fmt.Fprintln(w, "") // Trailing newline.
}
