package profile

import (
	"encoding/json"
	"os"
	"strings"
)

type Profile struct {
	ClaudeOAuthToken string `json:"claude_oauth_token"`
	GitHubToken      string `json:"github_token"`
}

type Store struct {
	path string
}

func NewStore(path string) *Store {
	return &Store{path: path}
}

func (s *Store) Load() (Profile, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return Profile{}, nil
	}
	if err != nil {
		return Profile{}, err
	}
	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return Profile{}, err
	}
	return p, nil
}

func (s *Store) Save(p Profile) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0600)
}

// MaskToken returns the token with all but the last 4 chars replaced with *.
func MaskToken(token string) string {
	if len(token) <= 4 {
		return token
	}
	return strings.Repeat("*", len(token)-4) + token[len(token)-4:]
}
