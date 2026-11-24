package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"unicode/utf8"

	_ "github.com/mattn/go-sqlite3"

	"github.com/nibzard/ohya/internal/antigravity"
	"github.com/nibzard/ohya/internal/store"
)

const conversationIndex = "conversation_summaries.db"

func antigravitySourceWorkspace(brainRoot string, log io.Writer) func(sourcePath string) (string, bool) {
	reported := false
	return func(sourcePath string) (string, bool) {
		path, found, err := conversationWorkspace(brainRoot, sourcePath)
		if err != nil && !reported {
			reported = true
			fmt.Fprintf(log, "ohya: %v; sessions stay without a working directory while the index cannot be read\n", err)
		}
		return path, found
	}
}

func conversationWorkspace(brainRoot, sourcePath string) (string, bool, error) {
	conversation := antigravity.ConversationIDFromPath(sourcePath)
	if conversation == "" {
		return "", false, nil
	}
	index, err := filepath.Abs(filepath.Join(filepath.Dir(brainRoot), conversationIndex))
	if err != nil {
		return "", false, fmt.Errorf("antigravity: resolve the conversation index: %w", err)
	}
	db, err := sql.Open("sqlite3", indexDSN(index))
	if err != nil {
		return "", false, fmt.Errorf("antigravity: open the conversation index %s: %w", index, err)
	}
	defer db.Close()
	var uris sql.NullString
	err = db.QueryRow(`SELECT workspace_uris FROM conversation_summaries WHERE conversation_id = ?`, conversation).Scan(&uris)
	switch {
	case errors.Is(err, sql.ErrNoRows), err == nil && !uris.Valid:
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("antigravity: read the conversation index %s: %w", index, err)
	}
	return firstLocalPath(uris.String)
}

func indexDSN(path string) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	u.RawQuery = "immutable=1"
	return u.String()
}

func firstLocalPath(list string) (string, bool, error) {
	var uris []string
	if err := json.Unmarshal([]byte(list), &uris); err != nil {
		return "", false, errors.New("antigravity: workspace_uris of a conversation is not a JSON list of strings")
	}
	for _, uri := range uris {
		if path, ok := localPath(uri); ok {
			return path, true, nil
		}
	}
	return "", false, nil
}

func localPath(uri string) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") {
		return "", false
	}
	path := filepath.Clean(u.Path)
	if !filepath.IsAbs(path) || len(path) > store.MaxWorkspacePathBytes || !utf8.ValidString(path) || strings.ContainsRune(path, 0) {
		return "", false
	}
	return path, true
}
