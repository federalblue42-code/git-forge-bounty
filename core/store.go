// The in-memory store: users, repositories, issues, pulls, labels, contents and
// git objects. Everything is deterministic: ids come from counters, timestamps
// from MAKEFUN_TRIAL_NOW, node ids from the id alone.
package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync"
)

// The suite's scenario is drawn from a published constant seed, so these logins
// are the same on every trial (wire.ts SCENARIO_SEED / generateUsers).
const (
	viewerLogin   = "adaiversen"
	viewerID      = 1000
	teammateLogin = "quinnkeller"
	strangerLogin = "olgabauer"
	orgLogin      = "faint-meadow-org"
)

type User struct {
	Login  string `json:"login"`
	ID     int64  `json:"id"`
	NodeID string `json:"node_id"`
	Name   string `json:"name"`
	Email  string `json:"email"`
	Type   string `json:"type"`
}

func userOf(login string, id int64) *User {
	return &User{
		Login: login, ID: id, NodeID: nodeID("U", id),
		Name: login, Email: login + "@example.invalid", Type: "User",
	}
}

var (
	viewer   = userOf(viewerLogin, viewerID)
	teammate = userOf(teammateLogin, 1001)
	stranger = userOf(strangerLogin, 1002)
)

func nodeID(prefix string, id int64) string {
	return prefix + "_" + base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(id, 10)))
}

// Object is one inflated git object.
type Object struct {
	Type string // blob | tree | commit
	Data []byte
}

type Label struct {
	ID          int64  `json:"id"`
	NodeID      string `json:"node_id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
	Default     bool   `json:"default"`
	URL         string `json:"url"`
}

type Issue struct {
	Number    int      `json:"number"`
	ID        int64    `json:"id"`
	NodeID    string   `json:"node_id"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	State     string   `json:"state"`
	Labels    []*Label `json:"labels"`
	User      *User    `json:"user"`
	Comments  int      `json:"comments"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
	ClosedAt  *string  `json:"closed_at"`
	HTMLURL   string   `json:"html_url"`
	URL       string   `json:"url"`
	RepoURL   string   `json:"repository_url"`
	Locked    bool     `json:"locked"`
}

type Pull struct {
	Number    int     `json:"number"`
	ID        int64   `json:"id"`
	NodeID    string  `json:"node_id"`
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	State     string  `json:"state"`
	Merged    bool    `json:"merged"`
	Mergeable bool    `json:"mergeable"`
	HeadRef   string  `json:"head_ref"`
	BaseRef   string  `json:"base_ref"`
	HeadSHA   string  `json:"head_sha"`
	BaseSHA   string  `json:"base_sha"`
	MergeSHA  string  `json:"merge_commit_sha"`
	User      *User   `json:"user"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	ClosedAt  *string `json:"closed_at"`
	MergedAt  *string `json:"merged_at"`
	HTMLURL   string  `json:"html_url"`
	URL       string  `json:"url"`
	Draft     bool    `json:"draft"`
}

type Content struct {
	Path   string
	SHA    string // blob oid
	Data   []byte
	Commit string // last commit oid touching it
}

type Repo struct {
	ID            int64
	NodeID        string
	Name          string
	FullName      string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedAt     string
	UpdatedAt     string
	PushedAt      string

	Refs       map[string]string  // full ref name -> commit oid
	Packs      map[string][]byte  // commit oid -> raw pack that introduced it (lazy)
	Objects    map[string]*Object // oid -> inflated object (cache)
	Commits    map[string]*CommitMeta
	Issues     []*Issue
	Pulls      []*Pull
	Labels     map[string]*Label
	Contents   map[string]*Content
	NextIssue  int
	NextPull   int
	fetchCache map[string][]byte // cached upload-pack responses, cleared on push
}

type CommitMeta struct {
	SHA     string
	Tree    string
	Parents []string
	Message string
}

type Store struct {
	mu       sync.Mutex
	now      string
	nextID   int64
	repos    map[string]*Repo // "owner/name" -> repo
	repoList []*Repo          // creation order
}

func NewStore(now string) *Store {
	return &Store{
		now:    now,
		nextID: 1,
		repos:  make(map[string]*Repo),
	}
}

func (s *Store) id() int64 {
	s.nextID++
	return s.nextID
}

func (s *Store) repo(owner, name string) *Repo {
	if owner != viewerLogin {
		return nil
	}
	return s.repos[owner+"/"+name]
}

func (s *Store) createRepo(name, description string, private bool) (*Repo, bool) {
	key := viewerLogin + "/" + name
	if _, exists := s.repos[key]; exists {
		return nil, false
	}
	r := &Repo{
		ID: s.id(), NodeID: nodeID("R", s.nextID),
		Name: name, FullName: key, Description: description, Private: private,
		DefaultBranch: "main",
		CreatedAt:     s.now, UpdatedAt: s.now, PushedAt: s.now,
		Refs:      make(map[string]string),
		Packs:     make(map[string][]byte),
		Objects:   make(map[string]*Object),
		Commits:   make(map[string]*CommitMeta),
		Labels:    make(map[string]*Label),
		Contents:  make(map[string]*Content),
		NextIssue: 1, NextPull: 1,
	}
	s.repos[key] = r
	s.repoList = append(s.repoList, r)
	return r, true
}

func (s *Store) deleteRepo(owner, name string) bool {
	key := owner + "/" + name
	r, ok := s.repos[key]
	if !ok {
		return false
	}
	delete(s.repos, key)
	for i, x := range s.repoList {
		if x == r {
			s.repoList = append(s.repoList[:i], s.repoList[i+1:]...)
			break
		}
	}
	return true
}

// ---------------------------------------------------------------- git objects

func hashObject(typ string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d", typ, len(data))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Store) putObject(r *Repo, typ string, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := r.Objects[oid]; !ok {
		r.Objects[oid] = &Object{Type: typ, Data: data}
	}
	return oid
}

// object fetches an object, inflating lazily from stored packs when needed.
func (s *Store) object(r *Repo, oid string) *Object {
	if o, ok := r.Objects[oid]; ok {
		return o
	}
	s.inflatePacks(r)
	return r.Objects[oid]
}
