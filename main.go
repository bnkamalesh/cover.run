package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/cache"
	"github.com/go-redis/redis"
	"github.com/gofn/gofn"
	"github.com/gofn/gofn/provision"
	"github.com/gorilla/mux"
	"github.com/urfave/negroni"
	msgpack "gopkg.in/vmihailenco/msgpack.v2"
)

const (
	// coverQMax is the maximum number of coverage run to be executed simultaneously
	coverQMax = 5
	// coverQName is the Redis channel name where the requests are queued
	coverQName = "coverqueue"

	// inProgrsKey is the redis HSet key in which all repo + tags which are currently being run
	// are saved
	inProgrsKey = "cover-in-progress"

	// DefaultTag is the Go version to run the tests with when no version
	// is specified
	DefaultTag = "1.10"
	// cacheExpiry is the duration in which the cache will be expired
	cacheExpiry = time.Hour
	// refreshWindows is the time duration, in which if the cache is about to expire
	// cover run is started again.
	refreshWindow = time.Minute * 10
)

var (
	// errLogger is the log instance with all the required flags set for error logging
	errLogger = log.New(os.Stderr, "Cover.Run ", log.LstdFlags|log.Lshortfile)

	// qLock is used to push to Redis channel because redis pub-sub in go-redis is
	// not concurrency safe
	qLock = sync.Mutex{}
	// qChan is used to control the number of simultaneos executions
	qChan = make(chan struct{}, coverQMax)

	httpClient = &http.Client{
		// img.shields.io response time is very slow
		Timeout: 30 * time.Second,
	}

	// ErrImgUnSupported is the error returned when the Go version requested is
	// not in the supported list
	ErrImgUnSupported = errors.New("Unsupported Go version provided")
	// ErrRepoNotFound is the error returned when the repository does not exist
	ErrRepoNotFound = errors.New("Repository not found")
	// ErrUnknown is the error returned when an unidentified error is encountered
	ErrUnknown = errors.New("Unknown error occurred")
	// ErrQueued is the error returned when the cover run queueu is full
	ErrQueued = errors.New("Request queued")
	// ErrCovInPrgrs is the error returned when the repo coverage test is in progress
	ErrCovInPrgrs = errors.New("Test in progress")
	// ErrNoTest is the error returned when no tests are found in the repository
	ErrNoTest = errors.New("No tests found")

	redisRing = redis.NewRing(&redis.RingOptions{
		Addrs: map[string]string{
			"server1": "redis:6379",
		},
	})
	redisCodec = &cache.Codec{
		Redis: redisRing,

		Marshal: func(v interface{}) ([]byte, error) {
			return msgpack.Marshal(v)
		},
		Unmarshal: func(b []byte, v interface{}) error {
			return msgpack.Unmarshal(b, v)
		},
	}
	redisClient = redis.NewClient(&redis.Options{
		Addr:         "redis:6379",
		ReadTimeout:  time.Second * 2,
		DialTimeout:  time.Second * 5,
		WriteTimeout: time.Second * 5,
		PoolTimeout:  time.Second * 120,
	})

	pageTmpl = template.Must(template.ParseFiles("./templates/page.tmpl"))

	// coverageMatch regex is used to match and find the coverage details from stdout
	coverageMatch = regexp.MustCompile("([coverage\\: ][0-9]+[.]?[0-9]*?[%])")
)

// goversionSupported returns true if the given Go version is supported
func goversionSupported(version string) bool {
	switch version {
	case "1.10",
		"1.9",
		"1.8":
		return true
	}
	return false
}

// repoExists checks if the given repository exists (works only if HTTP request returns 200)
func repoExists(repo string) (bool, error) {
	resp, err := httpClient.Get(fmt.Sprintf("https://%s", repo))
	if err != nil {
		return false, err
	}

	if resp.StatusCode == http.StatusNotFound {
		return false, ErrRepoNotFound
	}

	if resp.StatusCode > 399 {
		return false, ErrUnknown
	}
	return true, nil
}

// run runs the custom script to get the coverage details; using gofn
func run(goversion, repo string) (string, string, error) {
	_, err := repoExists(repo)
	if err != nil {
		return "", "", err
	}

	buildOpts := &provision.BuildOptions{
		DoNotUsePrefixImageName: true,
		ImageName:               strings.ToLower(fmt.Sprintf("bnkamalesh/cover.go:%s", goversion)),
		StdIN:                   fmt.Sprintf("sh /run.sh %s", repo),
	}

	// 5 minutes timeout
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*300)
	defer cancel()

	stdOut, stdErr, err := gofn.Run(ctx, buildOpts, &provision.ContainerOptions{})
	if err != nil {
		errLogger.Println(err, buildOpts)
	}

	return stdOut, stdErr, err
}

// Object struct holds all the details of a repository
type Object struct {
	Repo    string
	Tag     string
	Cover   string
	Output  bool
	AddedAt time.Time
}

// repoFullName generates a name by combining the Go tag
func repoFullName(repo, tag string, priority bool) string {
	if priority {
		return fmt.Sprintf("%s:%s-priority", repo, tag)
	}
	return fmt.Sprintf("%s:%s", repo, tag)
}

// repoTagFromFullName returns the repo name and Go tag, given the generated full name
func repoTagFromFullName(msg string) (string, string) {
	parts := strings.Split(msg, ":")
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "", ""
}

// addToQ pushes a new cover run request to the Redis channel
func addToQ(repo, tag string) error {
	qLock.Lock()
	err := redisClient.Publish(coverQName, repoFullName(repo, tag, false)).Err()
	qLock.Unlock()
	return err
}

// repoCoverStatus returns true if a repository + tag cover run is in progress
func repoCoverStatus(repo, tag string, priority bool) (bool, error) {
	// Check if cover run is already in progress for the given repo and tag
	err := redisRing.HGet(inProgrsKey, repoFullName(repo, tag, priority)).Err()
	if err == nil {
		return true, nil
	}
	errMsg := err.Error()
	if errMsg != "redis: nil" {
		errLogger.Println(errMsg)
	}

	return false, err
}

// setInProgress sets the repo + tag as in progress by adding to inPrgorsKey
func setInProgress(repo, tag string, priority bool) error {
	err := redisRing.HSet(inProgrsKey, repoFullName(repo, tag, priority), "y").Err()
	if err != nil {
		errLogger.Println(err)
	}
	return err
}

// unsetInProgress unsets the repo + tag from inprogress status
func unsetInProgress(repo, tag string, priority bool) error {
	err := redisRing.HDel(inProgrsKey, repoFullName(repo, tag, priority)).Err()
	if err != nil {
		errLogger.Println(err)
	}
	return err
}

// computeCoverage returns a string with the final computed coverage value
// Coverage percentages are read from the string output of go coverage
func computeCoverage(stdOut string) string {
	nn := coverageMatch.FindAllString(stdOut, -1)
	total := float64(0.00)
	count := float64(0.00)
	for _, n := range nn {
		n = strings.TrimSpace(n)
		n = strings.Trim(n, "%")
		f, err := strconv.ParseFloat(n, 64)
		if err != nil {
			continue
		}
		total += f
		count++
	}

	// to prevent divide by 0
	if count < 1.0 {
		count = 1.00
	}
	// rounding to 2
	return fmt.Sprintf("%.2f%%", (total / count))
}

// cover evaluates the coverage of a repository
// - Before starting evaluation, it sets the repo's status as in progress
// - Removes the inprogress status of a repo after it's done
func cover(repo, goversion string, priority bool) error {
	setInProgress(repo, goversion, priority)

	stdOut, stdErr, err := run(goversion, repo)
	if err != nil {
		errLogger.Println(err.Error())
		if len(stdErr) == 0 {
			stdErr = err.Error()
		}
	}

	unsetInProgress(repo, goversion, priority)

	obj := &Object{
		Repo:    repo,
		Tag:     goversion,
		Cover:   stdErr,
		Output:  false,
		AddedAt: time.Now(),
	}

	if stdOut != "" {
		obj.Cover = computeCoverage(stdOut)
		obj.Output = true
	}

	rerr := redisCodec.Set(&cache.Item{
		Key:        repoFullName(repo, goversion, priority),
		Object:     obj,
		Expiration: cacheExpiry,
	})
	if rerr != nil {
		errLogger.Println(rerr)
	}
	// if priority is true, then the request was not pushed to the Q channel,
	// So cleanup of channel is not required
	if !priority {
		<-qChan
	}

	if err == nil && obj.Cover == "" {
		return ErrNoTest
	}

	return err
}

// checkAndPush checks if cover can be run simultaneously, if not, then the request is pushed to Q
func checkAndPush(obj *Object) (*Object, error) {
	inprogress, err := repoCoverStatus(obj.Repo, obj.Tag, false)
	if err != nil {
		errMsg := err.Error()
		if errMsg != "redis: nil" {
			errLogger.Println(errMsg)
		}
	}

	if inprogress {
		obj.Cover = ErrCovInPrgrs.Error()
		return obj, ErrCovInPrgrs
	}
	err = addToQ(obj.Repo, obj.Tag)
	if err != nil {
		errLogger.Println(err)
		return obj, ErrUnknown
	}

	obj.Cover = ErrQueued.Error()

	return obj, ErrQueued
}

// repoCover returns code coverage details for the given repository and Go version
// - It checks if the coverage details is available in cache or not
// - It checks if the cover run is in progress or not
func repoCover(repo, imageTag string) (*Object, error) {
	obj := &Object{
		Repo: repo,
		Tag:  imageTag,
	}

	if !goversionSupported(imageTag) {
		obj.Cover = ErrImgUnSupported.Error()
		return obj, ErrImgUnSupported
	}

	err := redisCodec.Get(repoFullName(repo, imageTag, false), &obj)
	if err == nil {
		// Check if repo is about to expire, if yes then start cover run again to refresh cache.
		// This will help repositories with frequent requests to have the coverage details updated
		// and available all the time (if re-run is completed on time)
		now := time.Now()
		addedAt := obj.AddedAt
		addedAt = addedAt.Add(cacheExpiry)
		delta := addedAt.Sub(now)
		if delta > 0 && delta < refreshWindow {
			// In case of repos getting frequent checks, the re-run request will not
			// be queued and will be run immediately as high priority
			inprogress, _ := repoCoverStatus(obj.Repo, obj.Tag, true)
			if !inprogress {
				go cover(obj.Repo, obj.Tag, true)
			}
		}
		return obj, nil
	}
	errMsg := err.Error()
	if errMsg != "cache: key is missing" {
		errLogger.Println(errMsg)
	}

	return checkAndPush(obj)
}

type Repository struct {
	Repo  string
	Tag   string
	Cover string
}

func repoLatest() ([]*Repository, error) {
	repos := make([]*Repository, 0)
	keys, _, err := redisRing.Scan(0, "*", 10).Result()
	if err != nil {
		errLogger.Println(err)
		return repos, err
	}

	var obj Object
	for _, key := range keys {
		if len(repos) == 5 {
			return repos, nil
		}
		if err := redisCodec.Get(key, &obj); err == nil {
			if obj.Output {
				repos = append(repos, &Repository{obj.Repo, obj.Tag, obj.Cover})
			}
		}
	}
	return repos, nil
}

// subscribe subscribes to the Redis channel
func subscribe(qname string) {
	pubsub := redisClient.Subscribe(qname)
	defer pubsub.Close()

	for {
		msg, err := pubsub.ReceiveMessage()
		if err != nil {
			errLogger.Println(err)
		}
		repo, tag := repoTagFromFullName(msg.Payload)
		qChan <- struct{}{}
		go cover(repo, tag, false)
	}
}

func main() {
	r := mux.NewRouter()
	r.HandleFunc("/", Handler)
	r.PathPrefix("/assets").Handler(
		http.StripPrefix("/assets", http.FileServer(http.Dir("./assets/"))),
	)

	r.HandleFunc("/{repo:.*}.json", HandlerRepoJSON)
	r.HandleFunc("/{repo:.*}.svg", HandlerRepoSVG)

	go subscribe(coverQName)

	n := negroni.Classic()
	n.UseHandler(r)
	n.Run(":3000")
}
