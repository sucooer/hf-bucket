package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var apiClient = &http.Client{Timeout: 15 * time.Second}
var downloadClient = &http.Client{Timeout: 10 * time.Minute}

var defaultFolders = []string{"images", "videos", "documents", "others"}

const (
	defaultBucketName = "image"
	defaultStateFile  = "./data/state.json"
)

type Config struct {
	BotToken      string
	HFToken       string
	HFUsername    string
	CDNBaseURL    string
	Folders       []string
	Buckets       []string
	DefaultBucket string
	StateFile     string
}

type Bot struct {
	config           Config
	state            *StateStore
	availableBuckets []string
	hfUsername       string
}

type UserStats struct {
	Total           int    `json:"total"`
	Success         int    `json:"success"`
	Fail            int    `json:"fail"`
	PreferredBucket string `json:"preferred_bucket"`
}

type persistentState struct {
	Offset      int64                `json:"offset"`
	UserFolders map[int64]string     `json:"user_folders"`
	UserStats   map[int64]*UserStats `json:"user_stats"`
}

type StateStore struct {
	mu       sync.RWMutex
	filePath string
	state    persistentState
}

type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message,omitempty"`
}

type Message struct {
	MessageID int64       `json:"message_id"`
	Chat      Chat        `json:"chat"`
	From      *User       `json:"from,omitempty"`
	Text      string      `json:"text,omitempty"`
	Document  *Document   `json:"document,omitempty"`
	Photo     []PhotoSize `json:"photo,omitempty"`
	Video     *Video      `json:"video,omitempty"`
	Audio     *Audio      `json:"audio,omitempty"`
}

type Chat struct {
	ID int64 `json:"id"`
}

type User struct {
	ID int64 `json:"id"`
}

type Document struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
}

type PhotoSize struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
}

type Video struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name,omitempty"`
}

type Audio struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name,omitempty"`
}

type FileResponse struct {
	FilePath string `json:"file_path"`
}

type telegramBaseResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	ErrorCode   int    `json:"error_code"`
}

type GetUpdatesResponse struct {
	telegramBaseResponse
	Result []Update `json:"result"`
}

type GetFileResponse struct {
	telegramBaseResponse
	Result FileResponse `json:"result"`
}

type getMeResponse struct {
	telegramBaseResponse
	Result struct {
		Username string `json:"username"`
	} `json:"result"`
}

type hfWhoAmIResponse struct {
	Name string `json:"name"`
}

func loadConfig() (Config, error) {
	cfg := Config{
		BotToken:      strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		HFToken:       strings.TrimSpace(os.Getenv("HF_TOKEN")),
		HFUsername:    strings.TrimSpace(os.Getenv("HF_USERNAME")),
		CDNBaseURL:    strings.TrimRight(strings.TrimSpace(os.Getenv("CDN_BASE_URL")), "/"),
		Folders:       parseListEnv(os.Getenv("HF_FOLDERS"), defaultFolders),
		Buckets:       parseListEnv(os.Getenv("HF_BUCKETS"), nil),
		DefaultBucket: strings.TrimSpace(os.Getenv("HF_DEFAULT_BUCKET")),
		StateFile:     strings.TrimSpace(os.Getenv("STATE_FILE")),
	}

	if cfg.BotToken == "" {
		return Config{}, errors.New("TELEGRAM_BOT_TOKEN not set")
	}
	if cfg.HFToken == "" {
		return Config{}, errors.New("HF_TOKEN not set")
	}
	if cfg.DefaultBucket == "" {
		cfg.DefaultBucket = defaultBucketName
	}
	if cfg.StateFile == "" {
		cfg.StateFile = defaultStateFile
	}
	if len(cfg.Folders) == 0 {
		cfg.Folders = append([]string(nil), defaultFolders...)
	}
	return cfg, nil
}

func parseListEnv(raw string, fallback []string) []string {
	if strings.TrimSpace(raw) == "" {
		if fallback == nil {
			return nil
		}
		return append([]string(nil), fallback...)
	}

	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	seen := make(map[string]struct{})
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}

	if len(result) == 0 && fallback != nil {
		return append([]string(nil), fallback...)
	}
	return result
}

func defaultFolder(folders []string) string {
	for _, folder := range folders {
		if folder == "others" {
			return folder
		}
	}
	if len(folders) > 0 {
		return folders[0]
	}
	return "others"
}

func newStateStore(filePath string) (*StateStore, error) {
	store := &StateStore{
		filePath: filePath,
		state: persistentState{
			UserFolders: make(map[int64]string),
			UserStats:   make(map[int64]*UserStats),
		},
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return store, nil
		}
		return nil, fmt.Errorf("read state file: %w", err)
	}

	if len(bytes.TrimSpace(data)) == 0 {
		return store, nil
	}
	if err := json.Unmarshal(data, &store.state); err != nil {
		return nil, fmt.Errorf("decode state file: %w", err)
	}
	if store.state.UserFolders == nil {
		store.state.UserFolders = make(map[int64]string)
	}
	if store.state.UserStats == nil {
		store.state.UserStats = make(map[int64]*UserStats)
	}
	return store, nil
}

func (s *StateStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.filePath), 0755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}

	payload, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	tmpPath := s.filePath + ".tmp"
	if err := os.WriteFile(tmpPath, payload, 0644); err != nil {
		return fmt.Errorf("write state temp file: %w", err)
	}
	if err := os.Rename(tmpPath, s.filePath); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	return nil
}

func (s *StateStore) ensureUserLocked(userID int64, defaultFolder string) bool {
	changed := false
	if _, ok := s.state.UserStats[userID]; !ok {
		s.state.UserStats[userID] = &UserStats{}
		changed = true
	}
	if _, ok := s.state.UserFolders[userID]; !ok {
		s.state.UserFolders[userID] = defaultFolder
		changed = true
	}
	return changed
}

func (s *StateStore) EnsureUser(userID int64, defaultFolder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ensureUserLocked(userID, defaultFolder) {
		return nil
	}
	return s.saveLocked()
}

func (s *StateStore) GetUserFolder(userID int64, defaultFolder string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if folder, ok := s.state.UserFolders[userID]; ok && folder != "" {
		return folder
	}
	return defaultFolder
}

func (s *StateStore) SetUserFolder(userID int64, folder, defaultFolder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureUserLocked(userID, defaultFolder)
	s.state.UserFolders[userID] = folder
	return s.saveLocked()
}

func (s *StateStore) SetUserBucket(userID int64, bucket, defaultFolder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureUserLocked(userID, defaultFolder)
	s.state.UserStats[userID].PreferredBucket = bucket
	return s.saveLocked()
}

func (s *StateStore) GetUserBucket(userID int64, defaultFolder string, availableBuckets []string, fallback string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if stats, ok := s.state.UserStats[userID]; ok && stats.PreferredBucket != "" {
		if len(availableBuckets) == 0 || contains(availableBuckets, stats.PreferredBucket) {
			return stats.PreferredBucket
		}
	}
	if len(availableBuckets) > 0 {
		return availableBuckets[0]
	}
	if fallback != "" {
		return fallback
	}
	return defaultBucketName
}

func (s *StateStore) RecordAttempt(userID int64, defaultFolder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureUserLocked(userID, defaultFolder)
	s.state.UserStats[userID].Total++
	return s.saveLocked()
}

func (s *StateStore) RecordSuccess(userID int64, defaultFolder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureUserLocked(userID, defaultFolder)
	s.state.UserStats[userID].Success++
	return s.saveLocked()
}

func (s *StateStore) RecordFailure(userID int64, defaultFolder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureUserLocked(userID, defaultFolder)
	s.state.UserStats[userID].Fail++
	return s.saveLocked()
}

func (s *StateStore) GetUserStats(userID int64, defaultFolder string) UserStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stats, ok := s.state.UserStats[userID]
	if !ok || stats == nil {
		return UserStats{}
	}
	return *stats
}

func (s *StateStore) GetOffset() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.Offset
}

func (s *StateStore) SetOffset(offset int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Offset == offset {
		return nil
	}
	s.state.Offset = offset
	return s.saveLocked()
}

func newBot(cfg Config, state *StateStore) (*Bot, error) {
	username := cfg.HFUsername
	if username == "" {
		fetchedUsername, err := fetchUsername(cfg.HFToken)
		if err != nil {
			return nil, err
		}
		username = fetchedUsername
	}

	availableBuckets := append([]string(nil), cfg.Buckets...)
	if len(availableBuckets) == 0 {
		fetchedBuckets, err := fetchBuckets(username, cfg.HFToken)
		if err != nil {
			log.Printf("Warning: fetch buckets failed: %v", err)
		}
		availableBuckets = fetchedBuckets
	}
	if len(availableBuckets) == 0 {
		availableBuckets = []string{cfg.DefaultBucket}
	}

	log.Printf("Runtime config: username=%s buckets=%v folders=%v state=%s", username, availableBuckets, cfg.Folders, cfg.StateFile)

	return &Bot{
		config:           cfg,
		state:            state,
		availableBuckets: availableBuckets,
		hfUsername:       username,
	}, nil
}

func fetchUsername(hfToken string) (string, error) {
	req, err := http.NewRequest("GET", "https://huggingface.co/api/whoami-v2", nil)
	if err != nil {
		return "", fmt.Errorf("create whoami request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+hfToken)

	resp, err := apiClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch username: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("fetch username returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result hfWhoAmIResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode username response: %w", err)
	}
	if strings.TrimSpace(result.Name) == "" {
		return "", errors.New("whoami response did not include a username")
	}
	return strings.TrimSpace(result.Name), nil
}

func fetchBuckets(username, hfToken string) ([]string, error) {
	url := fmt.Sprintf("https://huggingface.co/api/buckets/%s", username)
	body, err := doHFRequestJSON(hfToken, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	var buckets []map[string]interface{}
	if err := json.Unmarshal(body, &buckets); err != nil {
		return nil, fmt.Errorf("decode buckets response: %w", err)
	}

	result := make([]string, 0, len(buckets))
	seen := make(map[string]struct{})
	for _, bucket := range buckets {
		id, ok := bucket["id"].(string)
		if !ok {
			continue
		}
		parts := strings.Split(id, "/")
		if len(parts) != 2 || strings.TrimSpace(parts[1]) == "" {
			continue
		}
		name := parts[1]
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	return result, nil
}

func doHFRequestJSON(hfToken, method, url string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+hfToken)

	resp, err := apiClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	return payload, nil
}

func (b *Bot) getMe() (string, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getMe", b.config.BotToken)
	resp, err := apiClient.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result getMeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if !result.OK {
		return "", fmt.Errorf("getMe failed: %s", result.Description)
	}
	return result.Result.Username, nil
}

func (b *Bot) setMyCommands() error {
	commands := []map[string]string{
		{"command": "start", "description": "启动 Bot"},
		{"command": "help", "description": "帮助信息"},
		{"command": "folder", "description": "切换文件夹"},
		{"command": "folders", "description": "查看所有文件夹"},
		{"command": "mkdir", "description": "创建或切换子目录"},
		{"command": "bucket", "description": "切换存储桶"},
		{"command": "buckets", "description": "查看所有存储桶"},
		{"command": "status", "description": "查看状态"},
		{"command": "stats", "description": "查看统计"},
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/setMyCommands", b.config.BotToken)
	bodyBytes, err := json.Marshal(map[string]interface{}{"commands": commands})
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := apiClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result telegramBaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("setMyCommands failed: %s", result.Description)
	}
	return nil
}

func (b *Bot) getUpdates(offset int64) ([]Update, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=10", b.config.BotToken, offset)
	resp, err := apiClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result GetUpdatesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if !result.OK {
		return nil, fmt.Errorf("getUpdates failed: %s", result.Description)
	}
	return result.Result, nil
}

func (b *Bot) getFile(fileID string) (string, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getFile?file_id=%s", b.config.BotToken, fileID)
	resp, err := apiClient.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result GetFileResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if !result.OK {
		return "", fmt.Errorf("getFile failed: %s", result.Description)
	}
	if strings.TrimSpace(result.Result.FilePath) == "" {
		return "", errors.New("getFile returned an empty file path")
	}
	return fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", b.config.BotToken, result.Result.FilePath), nil
}

func (b *Bot) sendMessage(chatID int64, text string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", b.config.BotToken)
	payload, err := json.Marshal(map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	})
	if err != nil {
		return err
	}

	resp, err := apiClient.Post(url, "application/json", bytes.NewBuffer(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result telegramBaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("sendMessage failed: %s", result.Description)
	}
	return nil
}

func parseCommand(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || !strings.HasPrefix(trimmed, "/") {
		return ""
	}
	command := strings.Fields(trimmed)[0]
	parts := strings.SplitN(command, "@", 2)
	return parts[0]
}

func parseCommandArgs(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || !strings.HasPrefix(trimmed, "/") {
		return ""
	}
	fields := strings.Fields(trimmed)
	if len(fields) < 2 {
		return ""
	}
	return strings.Join(fields[1:], " ")
}

func sanitizeFolderSegment(segment string) (string, error) {
	segment = strings.TrimSpace(segment)
	segment = strings.Trim(segment, "/")
	if segment == "" {
		return "", errors.New("folder segment is empty")
	}
	if segment == "." || segment == ".." {
		return "", errors.New("folder segment cannot be . or ..")
	}

	safe := sanitizePathComponent(segment)
	if safe == "" {
		return "", errors.New("folder segment is invalid after sanitization")
	}
	return safe, nil
}

func sanitizeSubPath(subPath string) (string, error) {
	normalized := strings.ReplaceAll(strings.TrimSpace(subPath), "\\", "/")
	normalized = strings.Trim(normalized, "/")
	if normalized == "" {
		return "", errors.New("subfolder path is empty")
	}

	parts := strings.Split(normalized, "/")
	safeParts := make([]string, 0, len(parts))
	for _, part := range parts {
		safePart, err := sanitizeFolderSegment(part)
		if err != nil {
			return "", err
		}
		safeParts = append(safeParts, safePart)
	}
	return strings.Join(safeParts, "/"), nil
}

func resolveFolderPathInput(input, currentFolder string, rootFolders []string) (string, error) {
	normalized := strings.ReplaceAll(strings.TrimSpace(input), "\\", "/")
	isAbsolute := strings.HasPrefix(normalized, "/")

	safePath, err := sanitizeSubPath(normalized)
	if err != nil {
		return "", err
	}

	parts := strings.Split(safePath, "/")
	if isAbsolute {
		if !contains(rootFolders, parts[0]) {
			return "", fmt.Errorf("top-level folder must be one of: %s", strings.Join(rootFolders, ", "))
		}
		return safePath, nil
	}

	if currentFolder == "" {
		currentFolder = defaultFolder(rootFolders)
	}
	return currentFolder + "/" + safePath, nil
}

func sanitizeFileName(fileName, fallback string) string {
	normalized := strings.ReplaceAll(strings.TrimSpace(fileName), "\\", "/")
	normalized = filepath.Base(normalized)
	if normalized == "." || normalized == string(filepath.Separator) || normalized == "" {
		normalized = fallback
	}
	if normalized == "" {
		normalized = "file"
	}

	safe := sanitizePathComponent(normalized)
	safe = strings.TrimSpace(safe)
	if safe == "" {
		safe = "file"
	}
	return safe
}

func sanitizePathComponent(value string) string {
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r == 0 || r < 32:
			builder.WriteByte('_')
		case strings.ContainsRune(`/\?#%*:|"<>`, r):
			builder.WriteByte('_')
		default:
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func encodePathSegments(parts ...string) string {
	encodedParts := make([]string, 0, len(parts))
	for _, part := range parts {
		for _, segment := range strings.Split(part, "/") {
			if segment == "" {
				continue
			}
			encodedParts = append(encodedParts, url.PathEscape(segment))
		}
	}
	return strings.Join(encodedParts, "/")
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (b *Bot) setUserFolder(userID int64, folder string) bool {
	if !contains(b.config.Folders, folder) {
		return false
	}
	if err := b.state.SetUserFolder(userID, folder, defaultFolder(b.config.Folders)); err != nil {
		log.Printf("Failed to persist folder selection: %v", err)
	}
	return true
}

func (b *Bot) setUserBucket(userID int64, bucket string) bool {
	if !contains(b.availableBuckets, bucket) {
		return false
	}
	if err := b.state.SetUserBucket(userID, bucket, defaultFolder(b.config.Folders)); err != nil {
		log.Printf("Failed to persist bucket selection: %v", err)
	}
	return true
}

func (b *Bot) uploadToHF(localPath, fileName, folder, bucket string) (string, error) {
	repoID := fmt.Sprintf("%s/%s", b.hfUsername, bucket)
	dstPath := fmt.Sprintf("hf://buckets/%s/%s/%s", repoID, folder, fileName)

	cmd := exec.Command("hf", "buckets", "cp", localPath, dstPath)
	cmd.Env = append(os.Environ(), "HF_TOKEN="+b.config.HFToken)

	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("HF CLI error: %s, output: %s", err, string(output))
		return "", fmt.Errorf("HF CLI error: %s", strings.TrimSpace(string(output)))
	}

	encodedObjectPath := encodePathSegments(folder, fileName)
	if b.config.CDNBaseURL != "" {
		return fmt.Sprintf("%s/%s/%s", b.config.CDNBaseURL, url.PathEscape(bucket), encodedObjectPath), nil
	}
	return fmt.Sprintf("https://huggingface.co/buckets/%s/resolve/%s", repoID, encodedObjectPath), nil
}

func (b *Bot) processFile(chatID, userID int64, fileName, folder, bucket, downloadURL string) {
	log.Printf("Processing file: %s bucket=%s folder=%s user=%d", fileName, bucket, folder, userID)

	defaultUserFolder := defaultFolder(b.config.Folders)
	if err := b.state.RecordAttempt(userID, defaultUserFolder); err != nil {
		log.Printf("Failed to persist attempt counter: %v", err)
	}

	if err := b.sendMessage(chatID, "⏳ 正在处理: "+fileName+"..."); err != nil {
		log.Printf("Failed to send processing message: %v", err)
	}

	tmpDir := "/tmp/hf_uploads"
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		log.Printf("Create temp dir failed: %v", err)
		if err := b.sendMessage(chatID, "❌ 创建临时目录失败: "+err.Error()); err != nil {
			log.Printf("Failed to send temp dir error: %v", err)
		}
		if err := b.state.RecordFailure(userID, defaultUserFolder); err != nil {
			log.Printf("Failed to persist failure counter: %v", err)
		}
		return
	}

	localName := fmt.Sprintf("%d_%d_%s", userID, time.Now().UnixNano(), fileName)
	localPath := filepath.Join(tmpDir, localName)

	resp, err := downloadClient.Get(downloadURL)
	if err != nil {
		log.Printf("Download failed: %v", err)
		if err := b.sendMessage(chatID, "❌ 下载失败: "+err.Error()); err != nil {
			log.Printf("Failed to send download error: %v", err)
		}
		if err := b.state.RecordFailure(userID, defaultUserFolder); err != nil {
			log.Printf("Failed to persist failure counter: %v", err)
		}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		message := fmt.Sprintf("下载失败，状态码 %d", resp.StatusCode)
		if len(body) > 0 {
			message += ": " + strings.TrimSpace(string(body))
		}
		log.Printf(message)
		if err := b.sendMessage(chatID, "❌ "+message); err != nil {
			log.Printf("Failed to send download status error: %v", err)
		}
		if err := b.state.RecordFailure(userID, defaultUserFolder); err != nil {
			log.Printf("Failed to persist failure counter: %v", err)
		}
		return
	}

	out, err := os.Create(localPath)
	if err != nil {
		log.Printf("Create file failed: %v", err)
		if err := b.sendMessage(chatID, "❌ 创建文件失败: "+err.Error()); err != nil {
			log.Printf("Failed to send create file error: %v", err)
		}
		if err := b.state.RecordFailure(userID, defaultUserFolder); err != nil {
			log.Printf("Failed to persist failure counter: %v", err)
		}
		return
	}

	_, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		os.Remove(localPath)
		log.Printf("Copy failed: %v", copyErr)
		if err := b.sendMessage(chatID, "❌ 下载失败: "+copyErr.Error()); err != nil {
			log.Printf("Failed to send copy error: %v", err)
		}
		if err := b.state.RecordFailure(userID, defaultUserFolder); err != nil {
			log.Printf("Failed to persist failure counter: %v", err)
		}
		return
	}
	if closeErr != nil {
		os.Remove(localPath)
		log.Printf("Close file failed: %v", closeErr)
		if err := b.sendMessage(chatID, "❌ 写入临时文件失败: "+closeErr.Error()); err != nil {
			log.Printf("Failed to send close file error: %v", err)
		}
		if err := b.state.RecordFailure(userID, defaultUserFolder); err != nil {
			log.Printf("Failed to persist failure counter: %v", err)
		}
		return
	}
	defer os.Remove(localPath)

	if err := b.sendMessage(chatID, "⏳ 上传到 "+bucket+"/"+folder+"/"+fileName+"..."); err != nil {
		log.Printf("Failed to send upload message: %v", err)
	}

	cdnURL, err := b.uploadToHF(localPath, fileName, folder, bucket)
	if err != nil {
		log.Printf("Upload failed: %v", err)
		if err := b.sendMessage(chatID, "❌ 上传失败: "+err.Error()); err != nil {
			log.Printf("Failed to send upload error: %v", err)
		}
		if err := b.state.RecordFailure(userID, defaultUserFolder); err != nil {
			log.Printf("Failed to persist failure counter: %v", err)
		}
		return
	}

	if err := b.state.RecordSuccess(userID, defaultUserFolder); err != nil {
		log.Printf("Failed to persist success counter: %v", err)
	}

	message := fmt.Sprintf("✅ 上传成功！\n\n📦 存储桶: %s\n📁 文件夹: %s\n🔗 %s", bucket, folder, cdnURL)
	if err := b.sendMessage(chatID, message); err != nil {
		log.Printf("Failed to send success message: %v", err)
	}
}

func actorIDFromMessage(msg *Message) int64 {
	if msg.From != nil && msg.From.ID != 0 {
		return msg.From.ID
	}
	return msg.Chat.ID
}

func (b *Bot) handleUpdate(update Update) {
	if update.Message == nil {
		return
	}

	msg := update.Message
	chatID := msg.Chat.ID
	userID := actorIDFromMessage(msg)
	defaultUserFolder := defaultFolder(b.config.Folders)

	if err := b.state.EnsureUser(userID, defaultUserFolder); err != nil {
		log.Printf("Failed to persist user state: %v", err)
	}

	trimmedText := strings.TrimSpace(msg.Text)
	command := parseCommand(msg.Text)
	if command != "" {
		switch command {
		case "/start":
			text := "👋 欢迎！我是 Hugging Face 上传 Bot\n\n发送文件直接上传\n/folder - 切换文件夹\n/mkdir - 创建或切换子目录\n/bucket - 切换存储桶\n/folders - 查看所有文件夹\n/buckets - 查看所有存储桶"
			if err := b.sendMessage(chatID, text); err != nil {
				log.Printf("Failed to send /start response: %v", err)
			}
			return
		case "/help":
			text := fmt.Sprintf("📖 使用帮助\n\n发送文件（图片、视频、音频、文档）我会自动上传到 Hugging Face Buckets\n\n目录示例：\n- 发送 `2024/01/` 追加子目录\n- 发送 `/images/2024/01/` 切到绝对目录\n- 发送 `/mkdir 2024/01/` 或 `/mkdir /images/2024/01/` 显式切换目录\n\n📦 存储桶：%s\n📁 文件夹：%s", strings.Join(b.availableBuckets, ", "), strings.Join(b.config.Folders, ", "))
			if err := b.sendMessage(chatID, text); err != nil {
				log.Printf("Failed to send /help response: %v", err)
			}
			return
		case "/folder":
			text := fmt.Sprintf("📁 当前文件夹: %s\n可选: %s\n\n发送 `年/月/` 追加子目录，或发送 `/images/2024/01/` 这样的绝对路径直接切换", b.state.GetUserFolder(userID, defaultUserFolder), strings.Join(b.config.Folders, ", "))
			if err := b.sendMessage(chatID, text); err != nil {
				log.Printf("Failed to send /folder response: %v", err)
			}
			return
		case "/mkdir":
			pathInput := parseCommandArgs(msg.Text)
			if pathInput == "" {
				text := "📁 用法：\n/mkdir 2024/01/\n/mkdir /images/2024/01/\n\n相对路径会追加到当前目录，绝对路径会从顶层目录开始切换。"
				if err := b.sendMessage(chatID, text); err != nil {
					log.Printf("Failed to send /mkdir usage: %v", err)
				}
				return
			}
			currentFolder := b.state.GetUserFolder(userID, defaultUserFolder)
			newFolder, err := resolveFolderPathInput(pathInput, currentFolder, b.config.Folders)
			if err != nil {
				if err := b.sendMessage(chatID, "❌ 目录不合法: "+err.Error()); err != nil {
					log.Printf("Failed to send /mkdir error: %v", err)
				}
				return
			}
			if err := b.state.SetUserFolder(userID, newFolder, defaultUserFolder); err != nil {
				log.Printf("Failed to persist /mkdir folder: %v", err)
				if err := b.sendMessage(chatID, "❌ 保存目录失败: "+err.Error()); err != nil {
					log.Printf("Failed to send /mkdir persistence error: %v", err)
				}
				return
			}
			if err := b.sendMessage(chatID, "📁 当前文件夹: "+newFolder); err != nil {
				log.Printf("Failed to send /mkdir success: %v", err)
			}
			return
		case "/folders":
			currentFolder := b.state.GetUserFolder(userID, defaultUserFolder)
			var builder strings.Builder
			builder.WriteString("📂 可用文件夹：\n")
			for _, folder := range b.config.Folders {
				mark := ""
				if folder == currentFolder {
					mark = " ✓"
				}
				builder.WriteString("• " + folder + mark + "\n")
			}
			if err := b.sendMessage(chatID, strings.TrimSpace(builder.String())); err != nil {
				log.Printf("Failed to send /folders response: %v", err)
			}
			return
		case "/bucket":
			text := fmt.Sprintf("📦 当前存储桶: %s\n可选: %s\n\n回复存储桶名切换", b.state.GetUserBucket(userID, defaultUserFolder, b.availableBuckets, b.config.DefaultBucket), strings.Join(b.availableBuckets, ", "))
			if err := b.sendMessage(chatID, text); err != nil {
				log.Printf("Failed to send /bucket response: %v", err)
			}
			return
		case "/buckets":
			currentBucket := b.state.GetUserBucket(userID, defaultUserFolder, b.availableBuckets, b.config.DefaultBucket)
			var builder strings.Builder
			builder.WriteString("📦 可用存储桶：\n")
			for _, bucket := range b.availableBuckets {
				mark := ""
				if bucket == currentBucket {
					mark = " ✓"
				}
				builder.WriteString("• " + bucket + mark + "\n")
			}
			if err := b.sendMessage(chatID, strings.TrimSpace(builder.String())); err != nil {
				log.Printf("Failed to send /buckets response: %v", err)
			}
			return
		case "/status":
			cdnValue := b.config.CDNBaseURL
			if cdnValue == "" {
				cdnValue = "未配置"
			}
			text := fmt.Sprintf("⚙️ 当前状态\n\n📦 存储桶: %s\n📁 文件夹: %s\n📂 可用: %s\n🔗 CDN: %s", b.state.GetUserBucket(userID, defaultUserFolder, b.availableBuckets, b.config.DefaultBucket), b.state.GetUserFolder(userID, defaultUserFolder), strings.Join(b.config.Folders, ", "), cdnValue)
			if err := b.sendMessage(chatID, text); err != nil {
				log.Printf("Failed to send /status response: %v", err)
			}
			return
		case "/stats":
			stats := b.state.GetUserStats(userID, defaultUserFolder)
			rate := 0.0
			if stats.Total > 0 {
				rate = float64(stats.Success) / float64(stats.Total) * 100
			}
			text := fmt.Sprintf("📊 上传统计\n\n总计: %d\n成功: %d\n失败: %d\n成功率: %.1f%%", stats.Total, stats.Success, stats.Fail, rate)
			if err := b.sendMessage(chatID, text); err != nil {
				log.Printf("Failed to send /stats response: %v", err)
			}
			return
		}
	}

	if trimmedText != "" && strings.HasSuffix(trimmedText, "/") {
		currentFolder := b.state.GetUserFolder(userID, defaultUserFolder)
		newFolder, err := resolveFolderPathInput(trimmedText, currentFolder, b.config.Folders)
		if err != nil {
			if err := b.sendMessage(chatID, "❌ 子目录不合法: "+err.Error()); err != nil {
				log.Printf("Failed to send invalid subfolder error: %v", err)
			}
			return
		}
		if err := b.state.SetUserFolder(userID, newFolder, defaultUserFolder); err != nil {
			log.Printf("Failed to persist subfolder: %v", err)
			if err := b.sendMessage(chatID, "❌ 保存子目录失败: "+err.Error()); err != nil {
				log.Printf("Failed to send subfolder persistence error: %v", err)
			}
			return
		}
		if err := b.sendMessage(chatID, "📁 当前文件夹: "+newFolder); err != nil {
			log.Printf("Failed to send subfolder update: %v", err)
		}
		return
	}

	if msg.Text != "" && !strings.HasPrefix(strings.TrimSpace(msg.Text), "/") {
		text := strings.TrimSpace(msg.Text)
		lowerText := strings.ToLower(text)
		if b.setUserBucket(userID, lowerText) {
			if err := b.sendMessage(chatID, "✅ 已切换到存储桶: "+lowerText); err != nil {
				log.Printf("Failed to send bucket update: %v", err)
			}
			return
		}
		if b.setUserFolder(userID, lowerText) {
			if err := b.sendMessage(chatID, "✅ 已切换到文件夹: "+lowerText); err != nil {
				log.Printf("Failed to send folder update: %v", err)
			}
			return
		}
		return
	}

	folder := b.state.GetUserFolder(userID, defaultUserFolder)
	bucket := b.state.GetUserBucket(userID, defaultUserFolder, b.availableBuckets, b.config.DefaultBucket)

	if msg.Document != nil {
		doc := msg.Document
		downloadURL, err := b.getFile(doc.FileID)
		if err != nil {
			log.Printf("Get document file failed: %v", err)
			if err := b.sendMessage(chatID, "❌ 获取文件地址失败: "+err.Error()); err != nil {
				log.Printf("Failed to send getFile document error: %v", err)
			}
			return
		}
		fileName := sanitizeFileName(doc.FileName, doc.FileUniqueID)
		go b.processFile(chatID, userID, fileName, folder, bucket, downloadURL)
		return
	}

	if len(msg.Photo) > 0 {
		photo := msg.Photo[len(msg.Photo)-1]
		downloadURL, err := b.getFile(photo.FileID)
		if err != nil {
			log.Printf("Get photo file failed: %v", err)
			if err := b.sendMessage(chatID, "❌ 获取图片地址失败: "+err.Error()); err != nil {
				log.Printf("Failed to send getFile photo error: %v", err)
			}
			return
		}
		fileName := sanitizeFileName(photo.FileUniqueID+".jpg", photo.FileUniqueID+".jpg")
		go b.processFile(chatID, userID, fileName, folder, bucket, downloadURL)
		return
	}

	if msg.Video != nil {
		video := msg.Video
		downloadURL, err := b.getFile(video.FileID)
		if err != nil {
			log.Printf("Get video file failed: %v", err)
			if err := b.sendMessage(chatID, "❌ 获取视频地址失败: "+err.Error()); err != nil {
				log.Printf("Failed to send getFile video error: %v", err)
			}
			return
		}
		fallbackName := "video_" + video.FileUniqueID + ".mp4"
		fileName := sanitizeFileName(video.FileName, fallbackName)
		go b.processFile(chatID, userID, fileName, folder, bucket, downloadURL)
		return
	}

	if msg.Audio != nil {
		audio := msg.Audio
		downloadURL, err := b.getFile(audio.FileID)
		if err != nil {
			log.Printf("Get audio file failed: %v", err)
			if err := b.sendMessage(chatID, "❌ 获取音频地址失败: "+err.Error()); err != nil {
				log.Printf("Failed to send getFile audio error: %v", err)
			}
			return
		}
		fallbackName := "audio_" + audio.FileUniqueID + ".mp3"
		fileName := sanitizeFileName(audio.FileName, fallbackName)
		go b.processFile(chatID, userID, fileName, folder, bucket, downloadURL)
		return
	}
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	state, err := newStateStore(cfg.StateFile)
	if err != nil {
		log.Fatalf("Failed to load state: %v", err)
	}

	bot, err := newBot(cfg, state)
	if err != nil {
		log.Fatalf("Failed to initialize bot: %v", err)
	}

	username, err := bot.getMe()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Bot started: @%s", username)

	if err := bot.setMyCommands(); err != nil {
		log.Printf("Warning: setMyCommands failed: %v", err)
	} else {
		log.Printf("Bot commands registered")
	}

	offset := state.GetOffset()
	for {
		updates, err := bot.getUpdates(offset)
		if err != nil {
			log.Printf("getUpdates error: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		for _, update := range updates {
			bot.handleUpdate(update)
			offset = update.UpdateID + 1
			if err := state.SetOffset(offset); err != nil {
				log.Printf("Failed to persist offset: %v", err)
			}
		}

		if len(updates) == 0 {
			time.Sleep(1 * time.Second)
		}
	}
}
