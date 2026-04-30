package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
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
	"sort"
	"strconv"
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
	sessionCookieName = "hf_web_session"
	sessionTTL        = 24 * time.Hour
	defaultWebListen  = ":8080"
)

type Config struct {
	BotToken             string
	HFToken              string
	HFUsername           string
	CDNBaseURL           string
	Folders              []string
	Buckets              []string
	DefaultBucket        string
	StateFile            string
	AllowedUserIDs       map[int64]struct{}
	AllowedChatIDs       map[int64]struct{}
	MaxConcurrentUploads int
	UploadQueueCapacity  int
	StateFlushInterval   time.Duration
	WebEnabled           bool
	WebListenAddr        string
	WebPassword          string
	WebBaseURL           string
}

type Bot struct {
	config           Config
	state            *StateStore
	availableBuckets []string
	hfUsername       string
	uploadQueue      chan UploadJob
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
	dirty    bool
	stopCh   chan struct{}
	wg       sync.WaitGroup
	interval time.Duration
}

type UploadJob struct {
	UserID      int64
	FileName    string
	Folder      string
	Bucket      string
	DownloadURL string
	LocalPath   string
	TrackStats  bool
	Notify      func(string)
	ResultCh    chan UploadResult
	Cleanup     func()
}

type UploadResult struct {
	FileName  string `json:"file_name"`
	Bucket    string `json:"bucket"`
	Folder    string `json:"folder"`
	CDNURL    string `json:"cdn_url"`
	DirectURL string `json:"direct_url"`
	Error     string `json:"error,omitempty"`
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

type webConfigResponse struct {
	Buckets       []webBucketInfo `json:"buckets"`
	Folders       []string        `json:"folders"`
	DefaultBucket string          `json:"default_bucket"`
	DefaultFolder string          `json:"default_folder"`
	WebBaseURL    string          `json:"web_base_url,omitempty"`
}

type webBucketInfo struct {
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	TotalFiles   int    `json:"total_files"`
	LastModified string `json:"last_modified"`
}

type bucketListEntry struct {
	Type       string `json:"type"`
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	MTime      string `json:"mtime"`
	UploadedAt string `json:"uploaded_at"`
}

type webFileEntry struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	UploadedAt string `json:"uploaded_at,omitempty"`
	MTime      string `json:"mtime,omitempty"`
	CDNURL     string `json:"cdn_url"`
	DirectURL  string `json:"direct_url"`
}

type webActionRequest struct {
	Bucket       string `json:"bucket"`
	Path         string `json:"path"`
	ConfirmName  string `json:"confirm_name,omitempty"`
	TargetPath   string `json:"target_path,omitempty"`
	TargetFolder string `json:"target_folder,omitempty"`
	TargetSubdir string `json:"target_subdir,omitempty"`
	TargetName   string `json:"target_name,omitempty"`
}

type webActionResponse struct {
	Success   bool   `json:"success"`
	Error     string `json:"error,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	Path      string `json:"path,omitempty"`
	NewPath   string `json:"new_path,omitempty"`
	CDNURL    string `json:"cdn_url,omitempty"`
	DirectURL string `json:"direct_url,omitempty"`
}

func loadConfig() (Config, error) {
	cfg := Config{
		BotToken:             strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		HFToken:              strings.TrimSpace(os.Getenv("HF_TOKEN")),
		HFUsername:           strings.TrimSpace(os.Getenv("HF_USERNAME")),
		CDNBaseURL:           strings.TrimRight(strings.TrimSpace(os.Getenv("CDN_BASE_URL")), "/"),
		Folders:              parseListEnv(os.Getenv("HF_FOLDERS"), defaultFolders),
		Buckets:              parseListEnv(os.Getenv("HF_BUCKETS"), nil),
		DefaultBucket:        strings.TrimSpace(os.Getenv("HF_DEFAULT_BUCKET")),
		StateFile:            strings.TrimSpace(os.Getenv("STATE_FILE")),
		AllowedUserIDs:       parseInt64SetEnv(os.Getenv("TELEGRAM_ALLOWED_USER_IDS")),
		AllowedChatIDs:       parseInt64SetEnv(os.Getenv("TELEGRAM_ALLOWED_CHAT_IDS")),
		MaxConcurrentUploads: parsePositiveIntEnv(os.Getenv("MAX_CONCURRENT_UPLOADS"), 2),
		StateFlushInterval:   parseDurationSecondsEnv(os.Getenv("STATE_FLUSH_INTERVAL_SECONDS"), 2*time.Second),
		WebEnabled:           parseBoolEnv(os.Getenv("WEB_ENABLED"), false),
		WebListenAddr:        strings.TrimSpace(os.Getenv("WEB_LISTEN_ADDR")),
		WebPassword:          os.Getenv("WEB_PASSWORD"),
		WebBaseURL:           strings.TrimRight(strings.TrimSpace(os.Getenv("WEB_BASE_URL")), "/"),
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
	if cfg.MaxConcurrentUploads < 1 {
		cfg.MaxConcurrentUploads = 1
	}
	cfg.UploadQueueCapacity = cfg.MaxConcurrentUploads * 4
	if cfg.UploadQueueCapacity < 8 {
		cfg.UploadQueueCapacity = 8
	}
	if cfg.WebListenAddr == "" {
		cfg.WebListenAddr = defaultWebListen
	}
	if cfg.WebEnabled && strings.TrimSpace(cfg.WebPassword) == "" {
		return Config{}, errors.New("WEB_ENABLED is true but WEB_PASSWORD is empty")
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

func parseInt64SetEnv(raw string) map[int64]struct{} {
	values := make(map[int64]struct{})
	for _, part := range strings.Split(raw, ",") {
		value := strings.TrimSpace(part)
		if value == "" {
			continue
		}
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			log.Printf("Warning: ignoring invalid int64 value %q", value)
			continue
		}
		values[id] = struct{}{}
	}
	return values
}

func parsePositiveIntEnv(raw string, fallback int) int {
	value := strings.TrimSpace(raw)
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		log.Printf("Warning: invalid positive int %q, using fallback %d", value, fallback)
		return fallback
	}
	return n
}

func parseDurationSecondsEnv(raw string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(raw)
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		log.Printf("Warning: invalid flush interval %q, using fallback %s", value, fallback)
		return fallback
	}
	return time.Duration(n) * time.Second
}

func parseBoolEnv(raw string, fallback bool) bool {
	value := strings.TrimSpace(strings.ToLower(raw))
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		log.Printf("Warning: invalid bool %q, using fallback %t", raw, fallback)
		return fallback
	}
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

func newStateStore(filePath string, flushInterval time.Duration) (*StateStore, error) {
	store := &StateStore{
		filePath: filePath,
		stopCh:   make(chan struct{}),
		interval: flushInterval,
		state: persistentState{
			UserFolders: make(map[int64]string),
			UserStats:   make(map[int64]*UserStats),
		},
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read state file: %w", err)
		}
	} else if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &store.state); err != nil {
			return nil, fmt.Errorf("decode state file: %w", err)
		}
	}

	if store.state.UserFolders == nil {
		store.state.UserFolders = make(map[int64]string)
	}
	if store.state.UserStats == nil {
		store.state.UserStats = make(map[int64]*UserStats)
	}

	store.wg.Add(1)
	go store.flushLoop()
	return store, nil
}

func (s *StateStore) writeStateLocked() error {
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
	s.dirty = false
	return nil
}

func (s *StateStore) markDirtyLocked() { s.dirty = true }

func (s *StateStore) flushLocked() error {
	if !s.dirty {
		return nil
	}
	return s.writeStateLocked()
}

func (s *StateStore) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

func (s *StateStore) flushLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := s.Flush(); err != nil {
				log.Printf("Failed to flush state: %v", err)
			}
		case <-s.stopCh:
			if err := s.Flush(); err != nil {
				log.Printf("Failed to flush state during shutdown: %v", err)
			}
			return
		}
	}
}

func (s *StateStore) Close() error {
	close(s.stopCh)
	s.wg.Wait()
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
	if s.ensureUserLocked(userID, defaultFolder) {
		s.markDirtyLocked()
	}
	return nil
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
	s.markDirtyLocked()
	return nil
}

func (s *StateStore) SetUserBucket(userID int64, bucket, defaultFolder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureUserLocked(userID, defaultFolder)
	s.state.UserStats[userID].PreferredBucket = bucket
	s.markDirtyLocked()
	return nil
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
	s.markDirtyLocked()
	return nil
}

func (s *StateStore) RecordSuccess(userID int64, defaultFolder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureUserLocked(userID, defaultFolder)
	s.state.UserStats[userID].Success++
	s.markDirtyLocked()
	return nil
}

func (s *StateStore) RecordFailure(userID int64, defaultFolder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureUserLocked(userID, defaultFolder)
	s.state.UserStats[userID].Fail++
	s.markDirtyLocked()
	return nil
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
	s.markDirtyLocked()
	return nil
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

	if len(cfg.AllowedUserIDs) == 0 && len(cfg.AllowedChatIDs) == 0 {
		log.Printf("Warning: no Telegram allowlist configured; the bot will accept updates from any user or chat")
	}

	log.Printf("Runtime config: username=%s buckets=%v folders=%v state=%s max_concurrent_uploads=%d web_enabled=%t", username, availableBuckets, cfg.Folders, cfg.StateFile, cfg.MaxConcurrentUploads, cfg.WebEnabled)

	bot := &Bot{
		config:           cfg,
		state:            state,
		availableBuckets: availableBuckets,
		hfUsername:       username,
		uploadQueue:      make(chan UploadJob, cfg.UploadQueueCapacity),
	}

	for i := 0; i < cfg.MaxConcurrentUploads; i++ {
		go bot.uploadWorker()
	}

	return bot, nil
}

func fetchUsername(hfToken string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, "https://huggingface.co/api/whoami-v2", nil)
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

func fetchBucketDetails(username, hfToken string) ([]webBucketInfo, error) {
	url := fmt.Sprintf("https://huggingface.co/api/buckets/%s", username)
	body, err := doHFRequestJSON(hfToken, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	var buckets []map[string]interface{}
	if err := json.Unmarshal(body, &buckets); err != nil {
		return nil, fmt.Errorf("decode buckets response: %w", err)
	}
	result := make([]webBucketInfo, 0, len(buckets))
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

		size, _ := bucket["size"].(float64)
		totalFiles, _ := bucket["totalFiles"].(float64)
		updatedAt, _ := bucket["updatedAt"].(string)

		result = append(result, webBucketInfo{
			Name:         name,
			Size:         int64(size),
			TotalFiles:   int(totalFiles),
			LastModified: updatedAt,
		})
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
	payload, err := json.Marshal(map[string]interface{}{"chat_id": chatID, "text": text})
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

func resolveWebFolder(baseFolder, subdir string, rootFolders []string) (string, error) {
	baseFolder = strings.TrimSpace(baseFolder)
	subdir = strings.TrimSpace(subdir)
	if baseFolder == "" {
		if subdir != "" {
			return "", errors.New("根目录下不能直接填写子目录，请先选择一个顶层目录")
		}
		return "", nil
	}
	if !contains(rootFolders, baseFolder) {
		return "", fmt.Errorf("目录必须是以下之一: %s", strings.Join(rootFolders, ", "))
	}
	if subdir == "" {
		return baseFolder, nil
	}
	safeSubdir, err := sanitizeSubPath(subdir)
	if err != nil {
		return "", err
	}
	return baseFolder + "/" + safeSubdir, nil
}

func resolveWebPath(input string) (string, error) {
	normalized := strings.ReplaceAll(strings.TrimSpace(input), "\\", "/")
	normalized = strings.Trim(normalized, "/")
	if normalized == "" {
		return "", nil
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

func containsID(values map[int64]struct{}, target int64) bool {
	_, ok := values[target]
	return ok
}

func (b *Bot) isAuthorized(chatID, userID int64) bool {
	if len(b.config.AllowedUserIDs) == 0 && len(b.config.AllowedChatIDs) == 0 {
		return true
	}
	return containsID(b.config.AllowedUserIDs, userID) || containsID(b.config.AllowedChatIDs, chatID)
}

func (b *Bot) uploadWorker() {
	for job := range b.uploadQueue {
		result := b.processUpload(job)
		if job.ResultCh != nil {
			job.ResultCh <- result
			close(job.ResultCh)
		}
	}
}

func (b *Bot) enqueueUpload(job UploadJob) bool {
	select {
	case b.uploadQueue <- job:
		return true
	default:
		return false
	}
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

func (b *Bot) buildUploadURLs(bucket, folder, fileName string) (string, string) {
	repoID := fmt.Sprintf("%s/%s", b.hfUsername, bucket)
	encodedObjectPath := encodePathSegments(folder, fileName)
	directURL := fmt.Sprintf("https://huggingface.co/buckets/%s/resolve/%s", repoID, encodedObjectPath)
	cdnURL := directURL
	if b.config.CDNBaseURL != "" {
		cdnURL = fmt.Sprintf("%s/%s/%s", b.config.CDNBaseURL, url.PathEscape(bucket), encodedObjectPath)
	}
	return cdnURL, directURL
}

func (b *Bot) buildPathURLs(bucket, objectPath string) (string, string) {
	repoID := fmt.Sprintf("%s/%s", b.hfUsername, bucket)
	encodedObjectPath := encodePathSegments(objectPath)
	directURL := fmt.Sprintf("https://huggingface.co/buckets/%s/resolve/%s", repoID, encodedObjectPath)
	cdnURL := directURL
	if b.config.CDNBaseURL != "" {
		cdnURL = fmt.Sprintf("%s/%s/%s", b.config.CDNBaseURL, url.PathEscape(bucket), encodedObjectPath)
	}
	return cdnURL, directURL
}

func (b *Bot) bucketObjectHandle(bucket, objectPath string) string {
	objectPath = strings.Trim(strings.TrimSpace(objectPath), "/")
	if objectPath == "" {
		return fmt.Sprintf("hf://buckets/%s/%s", b.hfUsername, bucket)
	}
	return fmt.Sprintf("hf://buckets/%s/%s/%s", b.hfUsername, bucket, objectPath)
}

func pathDir(path string) string {
	path = strings.Trim(strings.TrimSpace(path), "/")
	if path == "" {
		return ""
	}
	idx := strings.LastIndex(path, "/")
	if idx == -1 {
		return ""
	}
	return path[:idx]
}

func pathBase(path string) string {
	path = strings.Trim(strings.TrimSpace(path), "/")
	if path == "" {
		return ""
	}
	idx := strings.LastIndex(path, "/")
	if idx == -1 {
		return path
	}
	return path[idx+1:]
}

func joinObjectPath(dir, name string) string {
	dir = strings.Trim(strings.TrimSpace(dir), "/")
	name = strings.Trim(strings.TrimSpace(name), "/")
	switch {
	case dir == "":
		return name
	case name == "":
		return dir
	default:
		return dir + "/" + name
	}
}

func (b *Bot) hfBucketsCopy(srcBucket, srcPath, dstBucket, dstPath string) error {
	cmd := exec.Command("hf", "buckets", "cp", b.bucketObjectHandle(srcBucket, srcPath), b.bucketObjectHandle(dstBucket, dstPath))
	cmd.Env = append(os.Environ(), "HF_TOKEN="+b.config.HFToken)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("HF CLI copy error: %s, output: %s", err, string(output))
		return fmt.Errorf("HF CLI copy error: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

func (b *Bot) hfBucketsRemove(bucket, objectPath string) error {
	cmd := exec.Command("hf", "buckets", "remove", b.bucketObjectHandle(bucket, objectPath), "--yes")
	cmd.Env = append(os.Environ(), "HF_TOKEN="+b.config.HFToken)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("HF CLI remove error: %s, output: %s", err, string(output))
		return fmt.Errorf("HF CLI remove error: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

func (b *Bot) decodeActionRequest(r *http.Request) (webActionRequest, error) {
	var req webActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return webActionRequest{}, fmt.Errorf("invalid request body")
	}
	req.Bucket = strings.TrimSpace(req.Bucket)
	req.Path = strings.Trim(strings.TrimSpace(req.Path), "/")
	req.ConfirmName = strings.TrimSpace(req.ConfirmName)
	req.TargetPath = strings.TrimSpace(req.TargetPath)
	req.TargetFolder = strings.TrimSpace(req.TargetFolder)
	req.TargetSubdir = strings.TrimSpace(req.TargetSubdir)
	req.TargetName = strings.TrimSpace(req.TargetName)
	return req, nil
}

func (b *Bot) validateActionPath(bucket, objectPath string) error {
	if !contains(b.availableBuckets, bucket) {
		return errors.New("无效的存储桶")
	}
	if objectPath == "" {
		return errors.New("缺少文件路径")
	}
	return nil
}

func (b *Bot) uploadToHF(localPath, fileName, folder, bucket string) error {
	repoID := fmt.Sprintf("%s/%s", b.hfUsername, bucket)
	dstPath := fmt.Sprintf("hf://buckets/%s/%s/%s", repoID, folder, fileName)
	cmd := exec.Command("hf", "buckets", "cp", localPath, dstPath)
	cmd.Env = append(os.Environ(), "HF_TOKEN="+b.config.HFToken)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("HF CLI error: %s, output: %s", err, string(output))
		return fmt.Errorf("HF CLI error: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

func (b *Bot) listBucketFiles(bucket, prefix string) ([]webFileEntry, error) {
	bucketID := fmt.Sprintf("%s/%s", b.hfUsername, bucket)
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")

	cmd := exec.Command("hf", "buckets", "list", bucketID, "-R", "--format", "json")
	cmd.Env = append(os.Environ(), "HF_TOKEN="+b.config.HFToken)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("HF CLI list error: %s, output: %s", err, string(output))
		return nil, fmt.Errorf("HF CLI list error: %s", strings.TrimSpace(string(output)))
	}
	if len(bytes.TrimSpace(output)) == 0 {
		return []webFileEntry{}, nil
	}

	var raw []bucketListEntry
	if err := json.Unmarshal(output, &raw); err != nil {
		return nil, fmt.Errorf("decode bucket file list: %w", err)
	}

	files := make([]webFileEntry, 0, len(raw))
	for _, entry := range raw {
		if entry.Type != "file" || strings.TrimSpace(entry.Path) == "" {
			continue
		}
		if prefix != "" {
			if entry.Path != prefix && !strings.HasPrefix(entry.Path, prefix+"/") {
				continue
			}
		}
		cdnURL, directURL := b.buildPathURLs(bucket, entry.Path)
		files = append(files, webFileEntry{
			Path:       entry.Path,
			Size:       entry.Size,
			UploadedAt: entry.UploadedAt,
			MTime:      entry.MTime,
			CDNURL:     cdnURL,
			DirectURL:  directURL,
		})
	}
	sort.Slice(files, func(i, j int) bool {
		left := files[i].UploadedAt
		if left == "" {
			left = files[i].MTime
		}
		right := files[j].UploadedAt
		if right == "" {
			right = files[j].MTime
		}
		if left == right {
			return files[i].Path < files[j].Path
		}
		return left > right
	})
	return files, nil
}

func (b *Bot) processUpload(job UploadJob) UploadResult {
	result := UploadResult{FileName: job.FileName, Bucket: job.Bucket, Folder: job.Folder}
	if job.Cleanup != nil {
		defer job.Cleanup()
	}

	defaultUserFolder := defaultFolder(b.config.Folders)
	if job.TrackStats {
		if err := b.state.RecordAttempt(job.UserID, defaultUserFolder); err != nil {
			log.Printf("Failed to persist attempt counter: %v", err)
		}
	}
	if job.Notify != nil {
		job.Notify("⏳ 正在处理: " + job.FileName + "...")
	}

	localPath := job.LocalPath
	var removeTemp bool
	if localPath == "" {
		if err := os.MkdirAll("/tmp/hf_uploads", 0755); err != nil {
			result.Error = "创建临时目录失败: " + err.Error()
			return b.finalizeUploadFailure(job, result, defaultUserFolder)
		}
		tmpFile, err := os.CreateTemp("/tmp/hf_uploads", "tg-upload-*")
		if err != nil {
			result.Error = "创建临时文件失败: " + err.Error()
			return b.finalizeUploadFailure(job, result, defaultUserFolder)
		}
		localPath = tmpFile.Name()
		removeTemp = true

		resp, err := downloadClient.Get(job.DownloadURL)
		if err != nil {
			tmpFile.Close()
			result.Error = "下载失败: " + err.Error()
			return b.finalizeUploadFailure(job, result, defaultUserFolder)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			tmpFile.Close()
			result.Error = fmt.Sprintf("下载失败，状态码 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			return b.finalizeUploadFailure(job, result, defaultUserFolder)
		}
		if _, err := io.Copy(tmpFile, resp.Body); err != nil {
			tmpFile.Close()
			result.Error = "下载失败: " + err.Error()
			return b.finalizeUploadFailure(job, result, defaultUserFolder)
		}
		if err := tmpFile.Close(); err != nil {
			result.Error = "写入临时文件失败: " + err.Error()
			return b.finalizeUploadFailure(job, result, defaultUserFolder)
		}
	}
	if removeTemp {
		defer os.Remove(localPath)
	}

	if job.Notify != nil {
		job.Notify("⏳ 上传到 " + job.Bucket + "/" + job.Folder + "/" + job.FileName + "...")
	}
	if err := b.uploadToHF(localPath, job.FileName, job.Folder, job.Bucket); err != nil {
		result.Error = "上传失败: " + err.Error()
		return b.finalizeUploadFailure(job, result, defaultUserFolder)
	}
	if job.TrackStats {
		if err := b.state.RecordSuccess(job.UserID, defaultUserFolder); err != nil {
			log.Printf("Failed to persist success counter: %v", err)
		}
	}
	result.CDNURL, result.DirectURL = b.buildUploadURLs(job.Bucket, job.Folder, job.FileName)
	return result
}

func (b *Bot) finalizeUploadFailure(job UploadJob, result UploadResult, defaultUserFolder string) UploadResult {
	if job.TrackStats {
		if err := b.state.RecordFailure(job.UserID, defaultUserFolder); err != nil {
			log.Printf("Failed to persist failure counter: %v", err)
		}
	}
	return result
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

	if !b.isAuthorized(chatID, userID) {
		log.Printf("Ignoring unauthorized update from chat=%d user=%d", chatID, userID)
		return
	}
	if err := b.state.EnsureUser(userID, defaultUserFolder); err != nil {
		log.Printf("Failed to persist user state: %v", err)
	}

	trimmedText := strings.TrimSpace(msg.Text)
	command := parseCommand(msg.Text)
	if command != "" {
		switch command {
		case "/start":
			_ = b.sendMessage(chatID, "👋 欢迎！我是 Hugging Face 上传 Bot\n\n发送文件直接上传\n/folder - 切换文件夹\n/mkdir - 创建或切换子目录\n/bucket - 切换存储桶\n/folders - 查看所有文件夹\n/buckets - 查看所有存储桶")
			return
		case "/help":
			_ = b.sendMessage(chatID, fmt.Sprintf("📖 使用帮助\n\n发送文件（图片、视频、音频、文档）我会自动上传到 Hugging Face Buckets\n\n目录示例：\n- 发送 `2024/01/` 追加子目录\n- 发送 `/images/2024/01/` 切到绝对目录\n- 发送 `/mkdir 2024/01/` 或 `/mkdir /images/2024/01/` 显式切换目录\n\n📦 存储桶：%s\n📁 文件夹：%s", strings.Join(b.availableBuckets, ", "), strings.Join(b.config.Folders, ", ")))
			return
		case "/folder":
			_ = b.sendMessage(chatID, fmt.Sprintf("📁 当前文件夹: %s\n可选: %s\n\n发送 `年/月/` 追加子目录，或发送 `/images/2024/01/` 这样的绝对路径直接切换", b.state.GetUserFolder(userID, defaultUserFolder), strings.Join(b.config.Folders, ", ")))
			return
		case "/mkdir":
			pathInput := parseCommandArgs(msg.Text)
			if pathInput == "" {
				_ = b.sendMessage(chatID, "📁 用法：\n/mkdir 2024/01/\n/mkdir /images/2024/01/\n\n相对路径会追加到当前目录，绝对路径会从顶层目录开始切换。")
				return
			}
			currentFolder := b.state.GetUserFolder(userID, defaultUserFolder)
			newFolder, err := resolveFolderPathInput(pathInput, currentFolder, b.config.Folders)
			if err != nil {
				_ = b.sendMessage(chatID, "❌ 目录不合法: "+err.Error())
				return
			}
			if err := b.state.SetUserFolder(userID, newFolder, defaultUserFolder); err != nil {
				log.Printf("Failed to persist /mkdir folder: %v", err)
				_ = b.sendMessage(chatID, "❌ 保存目录失败: "+err.Error())
				return
			}
			_ = b.sendMessage(chatID, "📁 当前文件夹: "+newFolder)
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
			_ = b.sendMessage(chatID, strings.TrimSpace(builder.String()))
			return
		case "/bucket":
			_ = b.sendMessage(chatID, fmt.Sprintf("📦 当前存储桶: %s\n可选: %s\n\n回复存储桶名切换", b.state.GetUserBucket(userID, defaultUserFolder, b.availableBuckets, b.config.DefaultBucket), strings.Join(b.availableBuckets, ", ")))
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
			_ = b.sendMessage(chatID, strings.TrimSpace(builder.String()))
			return
		case "/status":
			cdnValue := b.config.CDNBaseURL
			if cdnValue == "" {
				cdnValue = "未配置"
			}
			_ = b.sendMessage(chatID, fmt.Sprintf("⚙️ 当前状态\n\n📦 存储桶: %s\n📁 文件夹: %s\n📂 可用: %s\n🔗 CDN: %s\n🚦 并发上传上限: %d", b.state.GetUserBucket(userID, defaultUserFolder, b.availableBuckets, b.config.DefaultBucket), b.state.GetUserFolder(userID, defaultUserFolder), strings.Join(b.config.Folders, ", "), cdnValue, b.config.MaxConcurrentUploads))
			return
		case "/stats":
			stats := b.state.GetUserStats(userID, defaultUserFolder)
			rate := 0.0
			if stats.Total > 0 {
				rate = float64(stats.Success) / float64(stats.Total) * 100
			}
			_ = b.sendMessage(chatID, fmt.Sprintf("📊 上传统计\n\n总计: %d\n成功: %d\n失败: %d\n成功率: %.1f%%", stats.Total, stats.Success, stats.Fail, rate))
			return
		}
	}

	if trimmedText != "" && strings.HasSuffix(trimmedText, "/") {
		currentFolder := b.state.GetUserFolder(userID, defaultUserFolder)
		newFolder, err := resolveFolderPathInput(trimmedText, currentFolder, b.config.Folders)
		if err != nil {
			_ = b.sendMessage(chatID, "❌ 子目录不合法: "+err.Error())
			return
		}
		if err := b.state.SetUserFolder(userID, newFolder, defaultUserFolder); err != nil {
			log.Printf("Failed to persist subfolder: %v", err)
			_ = b.sendMessage(chatID, "❌ 保存子目录失败: "+err.Error())
			return
		}
		_ = b.sendMessage(chatID, "📁 当前文件夹: "+newFolder)
		return
	}

	if msg.Text != "" && !strings.HasPrefix(strings.TrimSpace(msg.Text), "/") {
		text := strings.TrimSpace(msg.Text)
		lowerText := strings.ToLower(text)
		if b.setUserBucket(userID, lowerText) {
			_ = b.sendMessage(chatID, "✅ 已切换到存储桶: "+lowerText)
			return
		}
		if b.setUserFolder(userID, lowerText) {
			_ = b.sendMessage(chatID, "✅ 已切换到文件夹: "+lowerText)
			return
		}
		return
	}

	folder := b.state.GetUserFolder(userID, defaultUserFolder)
	bucket := b.state.GetUserBucket(userID, defaultUserFolder, b.availableBuckets, b.config.DefaultBucket)
	createTelegramJob := func(fileName, downloadURL string) UploadJob {
		notify := func(text string) {
			if err := b.sendMessage(chatID, text); err != nil {
				log.Printf("Failed to send upload status message: %v", err)
			}
		}
		return UploadJob{
			UserID:      userID,
			FileName:    fileName,
			Folder:      folder,
			Bucket:      bucket,
			DownloadURL: downloadURL,
			TrackStats:  true,
			Notify:      notify,
		}
	}

	if msg.Document != nil {
		doc := msg.Document
		downloadURL, err := b.getFile(doc.FileID)
		if err != nil {
			_ = b.sendMessage(chatID, "❌ 获取文件地址失败: "+err.Error())
			return
		}
		fileName := sanitizeFileName(doc.FileName, doc.FileUniqueID)
		job := createTelegramJob(fileName, downloadURL)
		if !b.enqueueUpload(job) {
			_ = b.sendMessage(chatID, "❌ 上传队列已满，请稍后再试")
			return
		}
		_ = b.sendMessage(chatID, "🕓 已加入上传队列: "+fileName)
		return
	}
	if len(msg.Photo) > 0 {
		photo := msg.Photo[len(msg.Photo)-1]
		downloadURL, err := b.getFile(photo.FileID)
		if err != nil {
			_ = b.sendMessage(chatID, "❌ 获取图片地址失败: "+err.Error())
			return
		}
		fileName := sanitizeFileName(photo.FileUniqueID+".jpg", photo.FileUniqueID+".jpg")
		job := createTelegramJob(fileName, downloadURL)
		if !b.enqueueUpload(job) {
			_ = b.sendMessage(chatID, "❌ 上传队列已满，请稍后再试")
			return
		}
		_ = b.sendMessage(chatID, "🕓 已加入上传队列: "+fileName)
		return
	}
	if msg.Video != nil {
		video := msg.Video
		downloadURL, err := b.getFile(video.FileID)
		if err != nil {
			_ = b.sendMessage(chatID, "❌ 获取视频地址失败: "+err.Error())
			return
		}
		fallbackName := "video_" + video.FileUniqueID + ".mp4"
		fileName := sanitizeFileName(video.FileName, fallbackName)
		job := createTelegramJob(fileName, downloadURL)
		if !b.enqueueUpload(job) {
			_ = b.sendMessage(chatID, "❌ 上传队列已满，请稍后再试")
			return
		}
		_ = b.sendMessage(chatID, "🕓 已加入上传队列: "+fileName)
		return
	}
	if msg.Audio != nil {
		audio := msg.Audio
		downloadURL, err := b.getFile(audio.FileID)
		if err != nil {
			_ = b.sendMessage(chatID, "❌ 获取音频地址失败: "+err.Error())
			return
		}
		fallbackName := "audio_" + audio.FileUniqueID + ".mp3"
		fileName := sanitizeFileName(audio.FileName, fallbackName)
		job := createTelegramJob(fileName, downloadURL)
		if !b.enqueueUpload(job) {
			_ = b.sendMessage(chatID, "❌ 上传队列已满，请稍后再试")
			return
		}
		_ = b.sendMessage(chatID, "🕓 已加入上传队列: "+fileName)
		return
	}
}

func (b *Bot) sessionSecret() []byte {
	return []byte(b.config.WebPassword + "|" + b.config.BotToken)
}

func (b *Bot) signSession(exp int64) string {
	payload := strconv.FormatInt(exp, 10)
	mac := hmac.New(sha256.New, b.sessionSecret())
	_, _ = mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

func (b *Bot) verifySession(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	mac := hmac.New(sha256.New, b.sessionSecret())
	_, _ = mac.Write([]byte(parts[0]))
	expected := hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(parts[1]), []byte(expected)) == 1
}

func requestIsSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (b *Bot) setSessionCookie(w http.ResponseWriter, r *http.Request) {
	expiresAt := time.Now().Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    b.signSession(expiresAt.Unix()),
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsSecure(r),
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsSecure(r),
	})
}

func (b *Bot) isWebAuthenticated(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	return b.verifySession(cookie.Value)
}

func (b *Bot) requireWebAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !b.isWebAuthenticated(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				respondJSON(w, http.StatusUnauthorized, map[string]interface{}{"success": false, "error": "需要先登录"})
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func respondJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("Failed to encode JSON response: %v", err)
	}
}

func (b *Bot) serveLoginPage(w http.ResponseWriter, r *http.Request) {
	if b.isWebAuthenticated(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, loginPageHTML)
}

func (b *Bot) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}
	password := r.FormValue("password")
	if subtle.ConstantTimeCompare([]byte(password), []byte(b.config.WebPassword)) != 1 {
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}
	b.setSessionCookie(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (b *Bot) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	clearSessionCookie(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (b *Bot) serveAppPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, appPageHTML)
}

func (b *Bot) handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	bucketDetails, _ := fetchBucketDetails(b.hfUsername, b.config.HFToken)
	if bucketDetails == nil {
		bucketDetails = []webBucketInfo{}
	}
	payload := webConfigResponse{
		Buckets:       bucketDetails,
		Folders:       b.config.Folders,
		DefaultBucket: b.state.GetUserBucket(0, defaultFolder(b.config.Folders), b.availableBuckets, b.config.DefaultBucket),
		DefaultFolder: defaultFolder(b.config.Folders),
		WebBaseURL:    b.config.WebBaseURL,
	}
	respondJSON(w, http.StatusOK, payload)
}

func (b *Bot) handleAPIUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "缺少上传文件"})
		return
	}
	defer file.Close()

	bucket := strings.TrimSpace(r.FormValue("bucket"))
	folder := strings.TrimSpace(r.FormValue("folder"))
	subdir := strings.TrimSpace(r.FormValue("subdir"))
	rawPath := strings.TrimSpace(r.FormValue("path"))
	if !contains(b.availableBuckets, bucket) {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "无效的存储桶"})
		return
	}
	resolvedFolder := ""
	if rawPath != "" {
		var err error
		resolvedFolder, err = resolveWebPath(rawPath)
		if err != nil {
			respondJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": err.Error()})
			return
		}
	} else {
		var err error
		resolvedFolder, err = resolveWebFolder(folder, subdir, b.config.Folders)
		if err != nil {
			respondJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": err.Error()})
			return
		}
	}
	fileName := sanitizeFileName(header.Filename, "upload.bin")

	if err := os.MkdirAll("/tmp/hf_uploads", 0755); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "error": "创建临时目录失败"})
		return
	}
	tmpFile, err := os.CreateTemp("/tmp/hf_uploads", "web-upload-*")
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "error": "创建临时文件失败"})
		return
	}
	if _, err := io.Copy(tmpFile, file); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "error": "保存上传文件失败"})
		return
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpFile.Name())
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "error": "写入上传文件失败"})
		return
	}

	resultCh := make(chan UploadResult, 1)
	job := UploadJob{
		FileName:   fileName,
		Folder:     resolvedFolder,
		Bucket:     bucket,
		LocalPath:  tmpFile.Name(),
		TrackStats: false,
		ResultCh:   resultCh,
		Cleanup: func() {
			_ = os.Remove(tmpFile.Name())
		},
	}
	if !b.enqueueUpload(job) {
		os.Remove(tmpFile.Name())
		respondJSON(w, http.StatusTooManyRequests, map[string]interface{}{"success": false, "error": "上传队列已满，请稍后再试"})
		return
	}
	result := <-resultCh
	if result.Error != "" {
		respondJSON(w, http.StatusBadGateway, map[string]interface{}{"success": false, "error": result.Error})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":    true,
		"file_name":  result.FileName,
		"bucket":     result.Bucket,
		"folder":     result.Folder,
		"cdn_url":    result.CDNURL,
		"direct_url": result.DirectURL,
	})
}

func (b *Bot) handleAPIFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	bucket := strings.TrimSpace(r.URL.Query().Get("bucket"))
	folder := strings.TrimSpace(r.URL.Query().Get("folder"))
	subdir := strings.TrimSpace(r.URL.Query().Get("subdir"))
	rawPath := strings.TrimSpace(r.URL.Query().Get("path"))
	if !contains(b.availableBuckets, bucket) {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "无效的存储桶"})
		return
	}
	resolvedFolder := ""
	if rawPath != "" {
		var err error
		resolvedFolder, err = resolveWebPath(rawPath)
		if err != nil {
			respondJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": err.Error()})
			return
		}
	} else if folder != "" || subdir != "" {
		var err error
		resolvedFolder, err = resolveWebFolder(folder, subdir, b.config.Folders)
		if err != nil {
			respondJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": err.Error()})
			return
		}
	}
	files, err := b.listBucketFiles(bucket, resolvedFolder)
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"bucket":  bucket,
		"folder":  resolvedFolder,
		"files":   files,
	})
}

func (b *Bot) handleAPIFileDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req, err := b.decodeActionRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, webActionResponse{Success: false, Error: err.Error()})
		return
	}
	if err := b.validateActionPath(req.Bucket, req.Path); err != nil {
		respondJSON(w, http.StatusBadRequest, webActionResponse{Success: false, Error: err.Error()})
		return
	}
	if req.ConfirmName != pathBase(req.Path) {
		respondJSON(w, http.StatusBadRequest, webActionResponse{Success: false, Error: "确认文件名不匹配"})
		return
	}
	if err := b.hfBucketsRemove(req.Bucket, req.Path); err != nil {
		respondJSON(w, http.StatusBadGateway, webActionResponse{Success: false, Error: err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, webActionResponse{Success: true, Bucket: req.Bucket, Path: req.Path})
}

func (b *Bot) handleAPIFileRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req, err := b.decodeActionRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, webActionResponse{Success: false, Error: err.Error()})
		return
	}
	if err := b.validateActionPath(req.Bucket, req.Path); err != nil {
		respondJSON(w, http.StatusBadRequest, webActionResponse{Success: false, Error: err.Error()})
		return
	}
	if req.ConfirmName != pathBase(req.Path) {
		respondJSON(w, http.StatusBadRequest, webActionResponse{Success: false, Error: "确认文件名不匹配"})
		return
	}
	newName := sanitizeFileName(req.TargetName, "")
	if newName == "" {
		respondJSON(w, http.StatusBadRequest, webActionResponse{Success: false, Error: "新文件名不能为空"})
		return
	}
	srcDir := pathDir(req.Path)
	newPath := joinObjectPath(srcDir, newName)
	if newPath == req.Path {
		respondJSON(w, http.StatusBadRequest, webActionResponse{Success: false, Error: "新文件名不能与原文件名相同"})
		return
	}
	if err := b.hfBucketsCopy(req.Bucket, req.Path, req.Bucket, newPath); err != nil {
		respondJSON(w, http.StatusBadGateway, webActionResponse{Success: false, Error: err.Error()})
		return
	}
	if err := b.hfBucketsRemove(req.Bucket, req.Path); err != nil {
		respondJSON(w, http.StatusBadGateway, webActionResponse{Success: false, Error: "目标文件已创建，但删除原文件失败: " + err.Error()})
		return
	}
	cdnURL, directURL := b.buildPathURLs(req.Bucket, newPath)
	respondJSON(w, http.StatusOK, webActionResponse{Success: true, Bucket: req.Bucket, Path: req.Path, NewPath: newPath, CDNURL: cdnURL, DirectURL: directURL})
}

func (b *Bot) handleAPIFileMove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req, err := b.decodeActionRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, webActionResponse{Success: false, Error: err.Error()})
		return
	}
	if err := b.validateActionPath(req.Bucket, req.Path); err != nil {
		respondJSON(w, http.StatusBadRequest, webActionResponse{Success: false, Error: err.Error()})
		return
	}
	if req.ConfirmName != pathBase(req.Path) {
		respondJSON(w, http.StatusBadRequest, webActionResponse{Success: false, Error: "确认文件名不匹配"})
		return
	}
	targetDir, err := resolveWebFolder(req.TargetFolder, req.TargetSubdir, b.config.Folders)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, webActionResponse{Success: false, Error: err.Error()})
		return
	}
	targetName := pathBase(req.Path)
	if strings.TrimSpace(req.TargetName) != "" {
		targetName = sanitizeFileName(req.TargetName, "")
	}
	newPath := joinObjectPath(targetDir, targetName)
	if newPath == req.Path {
		respondJSON(w, http.StatusBadRequest, webActionResponse{Success: false, Error: "目标路径不能与原路径相同"})
		return
	}
	if err := b.hfBucketsCopy(req.Bucket, req.Path, req.Bucket, newPath); err != nil {
		respondJSON(w, http.StatusBadGateway, webActionResponse{Success: false, Error: err.Error()})
		return
	}
	if err := b.hfBucketsRemove(req.Bucket, req.Path); err != nil {
		respondJSON(w, http.StatusBadGateway, webActionResponse{Success: false, Error: "目标文件已创建，但删除原文件失败: " + err.Error()})
		return
	}
	cdnURL, directURL := b.buildPathURLs(req.Bucket, newPath)
	respondJSON(w, http.StatusOK, webActionResponse{Success: true, Bucket: req.Bucket, Path: req.Path, NewPath: newPath, CDNURL: cdnURL, DirectURL: directURL})
}

func (b *Bot) handleAPIFileCopy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req, err := b.decodeActionRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, webActionResponse{Success: false, Error: err.Error()})
		return
	}
	if err := b.validateActionPath(req.Bucket, req.Path); err != nil {
		respondJSON(w, http.StatusBadRequest, webActionResponse{Success: false, Error: err.Error()})
		return
	}
	targetDir, err := resolveWebFolder(req.TargetFolder, req.TargetSubdir, b.config.Folders)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, webActionResponse{Success: false, Error: err.Error()})
		return
	}
	targetName := pathBase(req.Path)
	if strings.TrimSpace(req.TargetName) != "" {
		targetName = sanitizeFileName(req.TargetName, "")
	}
	newPath := joinObjectPath(targetDir, targetName)
	if newPath == req.Path {
		respondJSON(w, http.StatusBadRequest, webActionResponse{Success: false, Error: "目标路径不能与原路径相同"})
		return
	}
	if err := b.hfBucketsCopy(req.Bucket, req.Path, req.Bucket, newPath); err != nil {
		respondJSON(w, http.StatusBadGateway, webActionResponse{Success: false, Error: err.Error()})
		return
	}
	cdnURL, directURL := b.buildPathURLs(req.Bucket, newPath)
	respondJSON(w, http.StatusOK, webActionResponse{Success: true, Bucket: req.Bucket, Path: req.Path, NewPath: newPath, CDNURL: cdnURL, DirectURL: directURL})
}

func (b *Bot) newWebServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", b.requireWebAuth(b.serveAppPage))
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			b.handleLogin(w, r)
			return
		}
		b.serveLoginPage(w, r)
	})
	mux.HandleFunc("/logout", b.requireWebAuth(b.handleLogout))
	mux.HandleFunc("/api/config", b.requireWebAuth(b.handleAPIConfig))
	mux.HandleFunc("/api/upload", b.requireWebAuth(b.handleAPIUpload))
	mux.HandleFunc("/api/files", b.requireWebAuth(b.handleAPIFiles))
	mux.HandleFunc("/api/file/delete", b.requireWebAuth(b.handleAPIFileDelete))
	mux.HandleFunc("/api/file/rename", b.requireWebAuth(b.handleAPIFileRename))
	mux.HandleFunc("/api/file/move", b.requireWebAuth(b.handleAPIFileMove))
	mux.HandleFunc("/api/file/copy", b.requireWebAuth(b.handleAPIFileCopy))

	return &http.Server{Addr: b.config.WebListenAddr, Handler: mux}
}

const loginPageHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>HF 存储桶上传登录</title>
<style>
:root{--bg:#f5efe3;--panel:#fffdf8;--ink:#1f1a14;--muted:#6b5e52;--line:#d7cab6;--accent:#1e6a52;--accent-2:#d86f45}
*{box-sizing:border-box}body{margin:0;font-family:Georgia,"Times New Roman",serif;background:radial-gradient(circle at top,#fff6e6 0,#f1e6d2 45%,#e7dbc7 100%);color:var(--ink);min-height:100vh;display:grid;place-items:center;padding:24px}.card{width:min(420px,100%);background:rgba(255,253,248,.95);border:1px solid var(--line);border-radius:20px;padding:28px;box-shadow:0 24px 60px rgba(44,33,20,.12)}h1{margin:0 0 12px;font-size:32px;line-height:1}.meta{color:var(--muted);margin:0 0 24px;line-height:1.5}label{display:block;font-size:13px;text-transform:uppercase;letter-spacing:.08em;color:var(--muted);margin-bottom:8px}input{width:100%;padding:14px 16px;border-radius:12px;border:1px solid var(--line);font:inherit;background:#fff}button{margin-top:18px;width:100%;padding:14px 16px;border:0;border-radius:999px;background:linear-gradient(135deg,var(--accent),#0f4938);color:#fff;font:inherit;font-weight:700;cursor:pointer}.error{display:none;margin-bottom:16px;padding:12px 14px;border-radius:12px;background:#fff1eb;border:1px solid #f0b39b;color:#9a3412}
</style>
</head>
<body>
  <form class="card" method="post" action="/login">
    <h1>本地上传</h1>
    <p class="meta">输入共享密码后即可打开本地 Hugging Face Bucket 上传页面。</p>
    <div id="error" class="error">密码错误，请重试。</div>
    <label for="password">访问密码</label>
    <input id="password" name="password" type="password" autocomplete="current-password" required>
    <button type="submit">登录</button>
  </form>
<script>
if (new URLSearchParams(window.location.search).get('error')) {
  document.getElementById('error').style.display = 'block';
}
</script>
</body>
</html>`

const appPageHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Bucket 文件管理</title>
<style>
:root{--bg:#0f1216;--bg-2:#171b20;--panel:#1b2026;--panel-2:#232a31;--line:#2d353f;--text:#eef3f8;--muted:#94a1b2;--accent:#1994ff;--accent-2:#7c5cff;--green:#17c964;--yellow:#ffb020;--red:#ff5470;--cyan:#00b8d9;--shadow:0 24px 70px rgba(0,0,0,.34)}*{box-sizing:border-box}html,body{height:100%}body{margin:0;font-family:"SF Pro Display","PingFang SC","Noto Sans SC",sans-serif;background:linear-gradient(180deg,#0d1014 0,#131920 100%);color:var(--text)}button,input,select{font:inherit}.app-shell{min-height:100%;padding:18px}.app-frame{max-width:1320px;margin:0 auto;background:rgba(20,24,29,.96);border:1px solid rgba(255,255,255,.05);border-radius:28px;box-shadow:var(--shadow);overflow:hidden}.toolbar{display:grid;grid-template-columns:auto 1fr auto;gap:16px;align-items:center;padding:18px 22px;background:linear-gradient(180deg,rgba(255,255,255,.03),rgba(255,255,255,0));border-bottom:1px solid var(--line)}.brand{font-size:24px;font-weight:700;letter-spacing:.02em}.toolbar-main{display:grid;grid-template-columns:180px 180px 1fr auto auto;gap:12px;align-items:center}.toolbar input,.toolbar select{width:100%;padding:12px 14px;border-radius:14px;border:1px solid var(--line);background:var(--panel);color:var(--text);outline:none}.toolbar-actions{display:flex;align-items:center;gap:12px}.btn{border:0;border-radius:999px;padding:11px 16px;cursor:pointer;transition:.18s ease;display:inline-flex;align-items:center;gap:8px}.btn:hover{transform:translateY(-1px)}.btn-primary{background:linear-gradient(135deg,var(--accent),#006eff);color:#fff}.btn-secondary{background:var(--panel-2);color:var(--text);border:1px solid var(--line)}.btn-ghost{background:transparent;color:var(--muted);border:1px solid var(--line)}.view-switch{display:flex;gap:8px;padding:4px;background:var(--panel);border:1px solid var(--line);border-radius:999px}.view-btn{border:0;background:transparent;color:var(--muted);padding:8px 12px;border-radius:999px;cursor:pointer}.view-btn.active{background:var(--panel-2);color:var(--text)}.content{padding:18px 22px 26px}.content-head{display:flex;justify-content:space-between;gap:16px;align-items:center;margin-bottom:14px}.crumbs{display:flex;align-items:center;gap:8px;flex-wrap:wrap;color:var(--muted);font-size:14px}.crumbs strong{color:var(--text);font-weight:600}.status-line{font-size:14px;color:var(--muted)}.file-list{display:grid;gap:12px}.file-list.view-grid{grid-template-columns:repeat(auto-fill,minmax(220px,1fr));gap:14px}.file-list.view-image{grid-template-columns:repeat(auto-fill,minmax(180px,1fr));gap:14px}.file-row{position:relative;display:grid;grid-template-columns:auto 1fr auto auto;gap:14px;align-items:center;padding:16px 18px;border-radius:22px;background:rgba(255,255,255,.03);border:1px solid transparent;transition:.18s ease}.file-row:hover{background:rgba(255,255,255,.055);border-color:rgba(255,255,255,.05)}.file-row.active{background:rgba(255,255,255,.08);border-color:rgba(255,255,255,.06)}.file-list.view-grid .file-row,.file-list.view-image .file-row{grid-template-columns:1fr;align-items:flex-start;padding:14px}.file-list.view-image .file-row{padding:12px}.thumb{width:52px;height:52px;border-radius:16px;background:linear-gradient(180deg,#1f8fff,#0073ff);display:grid;place-items:center;font-size:26px;flex:none;overflow:hidden}.thumb img{width:100%;height:100%;object-fit:cover;display:block}.file-list.view-grid .thumb{width:100%;aspect-ratio:1.2;border-radius:18px;font-size:34px}.file-list.view-image .thumb{width:100%;aspect-ratio:1;border-radius:18px;font-size:34px}.file-body{min-width:0}.file-name{font-size:18px;font-weight:600;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.file-list.view-grid .file-name,.file-list.view-image .file-name{margin-top:12px;white-space:normal;word-break:break-all;line-height:1.35;font-size:16px}.file-meta{margin-top:6px;color:var(--muted);font-size:13px;display:flex;gap:10px;flex-wrap:wrap}.file-size{font-size:16px;color:var(--text);font-weight:600;justify-self:end}.file-list.view-grid .file-size,.file-list.view-image .file-size{justify-self:start;margin-top:8px}.more-btn{width:42px;height:42px;border:0;border-radius:999px;background:rgba(255,255,255,.05);color:var(--accent);font-size:22px;cursor:pointer}.more-btn:hover{background:rgba(25,148,255,.15)}.menu{position:fixed;min-width:240px;background:rgba(20,24,29,.98);border:1px solid rgba(255,255,255,.07);border-radius:22px;box-shadow:0 28px 80px rgba(0,0,0,.45);padding:10px;display:none;z-index:40}.menu.open{display:block}.menu button{width:100%;display:flex;align-items:center;gap:14px;border:0;background:transparent;color:var(--text);padding:14px 14px;border-radius:16px;text-align:left;cursor:pointer}.menu button:hover{background:rgba(255,255,255,.06)}.menu .icon{width:28px;text-align:center;font-size:22px}.menu .rename .icon{color:var(--accent-2)}.menu .move .icon{color:var(--yellow)}.menu .copy .icon{color:var(--green)}.menu .delete .icon{color:var(--red)}.menu .share .icon{color:var(--cyan)}.menu .copy-link .icon{color:var(--accent)}.menu .download .icon{color:var(--cyan)}.empty{padding:40px 24px;border-radius:24px;border:1px dashed rgba(255,255,255,.12);background:rgba(255,255,255,.02);text-align:center;color:var(--muted)}.overlay{position:fixed;inset:0;background:rgba(0,0,0,.58);backdrop-filter:blur(8px);display:none;align-items:center;justify-content:center;padding:24px;z-index:50}.overlay.open{display:flex}.dialog{width:min(520px,100%);background:#171b20;border:1px solid rgba(255,255,255,.08);border-radius:26px;padding:24px;box-shadow:var(--shadow)}.dialog h3{margin:0 0 10px;font-size:24px}.dialog p{margin:0 0 18px;color:var(--muted);line-height:1.6}.dialog .field{margin-bottom:14px}.dialog label{display:block;font-size:12px;letter-spacing:.08em;text-transform:uppercase;color:var(--muted);margin-bottom:8px}.dialog input,.dialog select{width:100%;padding:13px 14px;border-radius:14px;border:1px solid var(--line);background:var(--panel);color:var(--text)}.dialog-actions{display:flex;justify-content:flex-end;gap:12px;margin-top:18px}.uploader{width:min(640px,100%)}.form-grid{display:grid;grid-template-columns:1fr 1fr;gap:12px}.form-grid .full{grid-column:1/-1}.hidden{display:none!important}.file-list.view-image .file-meta{margin-top:8px}.file-list.view-image .file-row .more-btn,.file-list.view-grid .file-row .more-btn{position:absolute;top:12px;right:12px}@media (max-width:980px){.toolbar{grid-template-columns:1fr;align-items:stretch}.toolbar-main{grid-template-columns:1fr 1fr;grid-auto-rows:auto}.toolbar-main .toolbar-wide{grid-column:1/-1}.toolbar-actions{justify-content:space-between}.content-head{flex-direction:column;align-items:flex-start}.file-row{grid-template-columns:auto 1fr auto}.file-size{display:none}.file-list.view-grid,.file-list.view-image{grid-template-columns:repeat(auto-fill,minmax(150px,1fr))}.form-grid{grid-template-columns:1fr}}
</style>
</head>
<body>
<div class="app-shell">
  <div class="app-frame">
    <div class="toolbar">
      <div class="brand">文件管理</div>
      <div class="toolbar-main">
        <button id="back-to-buckets" class="btn btn-secondary" type="button" style="display:none">← 返回存储桶</button>
        <button id="refresh-files" class="btn btn-secondary" type="button">刷新</button>
        <div class="view-switch" aria-label="视图切换">
          <button class="view-btn active" type="button" data-view="list">列表</button>
          <button class="view-btn" type="button" data-view="grid">网格</button>
          <button class="view-btn" type="button" data-view="image">图片</button>
        </div>
      </div>
      <div class="toolbar-actions">
        <button id="open-upload" class="btn btn-primary" type="button">上传</button>
        <form method="post" action="/logout"><button class="btn btn-ghost" type="submit">退出</button></form>
      </div>
    </div>
    <div class="content">
      <div class="content-head">
        <div id="crumbs" class="crumbs"></div>
        <div id="browser-status" class="status-line"></div>
      </div>
      <div id="file-list" class="file-list view-list"></div>
    </div>
  </div>
</div>

<div id="action-menu" class="menu" role="menu" aria-hidden="true">
  <button class="rename" type="button" data-action="rename"><span class="icon">⟲</span><span>重命名</span></button>
  <button class="move" type="button" data-action="move"><span class="icon">↪</span><span>移动</span></button>
  <button class="copy" type="button" data-action="copy"><span class="icon">⧉</span><span>复制</span></button>
  <button class="delete" type="button" data-action="delete"><span class="icon">🗑</span><span>删除</span></button>
  <button class="share" type="button" data-action="share"><span class="icon">⇪</span><span>分享</span></button>
  <button class="copy-link" type="button" data-action="copy-cdn"><span class="icon">🔗</span><span>复制链接</span></button>
  <button class="copy-link" type="button" data-action="copy-direct"><span class="icon">⊹</span><span>复制直链</span></button>
  <button class="download" type="button" data-action="download"><span class="icon">⤓</span><span>下载</span></button>
</div>

<div id="dialog-overlay" class="overlay" aria-hidden="true">
  <div class="dialog">
    <h3 id="dialog-title"></h3>
    <p id="dialog-description"></p>
    <div id="dialog-fields"></div>
    <div class="dialog-actions">
      <button id="dialog-cancel" class="btn btn-ghost" type="button">取消</button>
      <button id="dialog-submit" class="btn btn-primary" type="button">确认</button>
    </div>
  </div>
</div>

<div id="upload-overlay" class="overlay" aria-hidden="true">
  <div class="dialog uploader">
    <h3>上传文件</h3>
    <p>上传成功后会自动刷新当前文件列表。</p>
    <form id="upload-form">
      <div class="form-grid">
        <div class="field">
          <label for="upload-bucket">存储桶</label>
          <select id="upload-bucket" name="bucket"></select>
        </div>
        <div class="field">
          <label for="upload-folder">顶层目录</label>
          <select id="upload-folder" name="folder"></select>
        </div>
        <div class="field full">
          <label for="upload-subdir">可选子目录</label>
          <input id="upload-subdir" name="subdir" type="text" placeholder="例如 2024/01 或 raw/mobile">
        </div>
        <div class="field full">
          <label for="upload-file">文件</label>
          <input id="upload-file" name="file" type="file" required>
        </div>
      </div>
      <div id="upload-status" class="status-line" style="margin-top:12px"></div>
      <div id="upload-result" class="result"></div>
      <div class="dialog-actions">
        <button id="upload-cancel" class="btn btn-ghost" type="button">关闭</button>
        <button id="upload-submit" class="btn btn-primary" type="submit">开始上传</button>
      </div>
    </form>
  </div>
</div>

<script>
const state = {
  config: null,
  files: [],
  fileMap: new Map(),
  buckets: [],
  currentBucket: '',
  currentFolder: '',
  currentSubdir: '',
  viewingBuckets: true,
  view: 'list',
  menuPath: '',
  actionFile: null,
  actionType: ''
};

const els = {
  backToBuckets: document.getElementById('back-to-buckets'),
  refreshFiles: document.getElementById('refresh-files'),
  viewButtons: Array.from(document.querySelectorAll('.view-btn')),
  fileList: document.getElementById('file-list'),
  crumbs: document.getElementById('crumbs'),
  browserStatus: document.getElementById('browser-status'),
  actionMenu: document.getElementById('action-menu'),
  dialogOverlay: document.getElementById('dialog-overlay'),
  dialogTitle: document.getElementById('dialog-title'),
  dialogDescription: document.getElementById('dialog-description'),
  dialogFields: document.getElementById('dialog-fields'),
  dialogSubmit: document.getElementById('dialog-submit'),
  dialogCancel: document.getElementById('dialog-cancel'),
  uploadOverlay: document.getElementById('upload-overlay'),
  openUpload: document.getElementById('open-upload'),
  uploadCancel: document.getElementById('upload-cancel'),
  uploadForm: document.getElementById('upload-form'),
  uploadBucket: document.getElementById('upload-bucket'),
  uploadFile: document.getElementById('upload-file'),
  uploadStatus: document.getElementById('upload-status'),
  uploadResult: document.getElementById('upload-result')
};

function escapeHtml(value) {
  return String(value).replace(/[&<>"']/g, function (char) {
    return ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'})[char];
  });
}

function basename(filePath) {
  return filePath.split('/').filter(Boolean).pop() || filePath;
}

function dirname(filePath) {
  const parts = filePath.split('/').filter(Boolean);
  parts.pop();
  return parts.join('/');
}

function joinPath(dir, name) {
  dir = (dir || '').replace(/^\/+|\/+$/g, '');
  name = (name || '').replace(/^\/+|\/+$/g, '');
  if (!dir) return name;
  if (!name) return dir;
  return dir + '/' + name;
}

function formatBytes(value) {
  if (typeof value !== 'number' || Number.isNaN(value)) return '';
  const units = ['B', 'KB', 'MB', 'GB'];
  let size = value;
  let idx = 0;
  while (size >= 1024 && idx < units.length - 1) {
    size /= 1024;
    idx += 1;
  }
  const fixed = idx === 0 ? 0 : 2;
  return size.toFixed(fixed) + units[idx];
}

function formatTime(file) {
  return file.uploaded_at || file.mtime || '';
}

function isImageFile(path) {
  const lower = path.toLowerCase();
  return ['.jpg','.jpeg','.png','.gif','.webp','.avif','.bmp','.svg'].some(ext => lower.endsWith(ext));
}

function fileIcon(path) {
  if (isImageFile(path)) return '🖼';
  if (/\.(mp4|mov|mkv|webm)$/i.test(path)) return '🎬';
  if (/\.(mp3|flac|wav|m4a)$/i.test(path)) return '🎵';
  if (/\.(zip|rar|7z|tar|gz)$/i.test(path)) return '🗜';
  return '📄';
}

function showUploadResult(kind, html) {
  els.uploadResult.className = 'result ' + kind;
  els.uploadResult.style.display = 'block';
  els.uploadResult.innerHTML = html;
}

function clearUploadResult() {
  els.uploadResult.style.display = 'none';
  els.uploadResult.innerHTML = '';
}

function setView(view) {
  state.view = view;
  els.viewButtons.forEach(btn => btn.classList.toggle('active', btn.dataset.view === view));
  els.fileList.className = 'file-list view-' + view;
  renderFiles();
}

function renderBreadcrumbs() {
  const parts = [];
  if (state.viewingBuckets) {
    parts.push('<span class="current">我的存储桶</span>');
  } else {
    parts.push('<span class="crumb-link" data-crumb="buckets">我的存储桶</span>');
    parts.push('<span class="separator">/</span>');
    parts.push('<strong>' + escapeHtml(state.currentBucket || '') + '</strong>');
    if (!state.currentFolder) {
      parts.push('<span>/</span><span>根目录</span>');
    } else {
      state.currentFolder.split('/').forEach(seg => {
        parts.push('<span>/</span><span>' + escapeHtml(seg) + '</span>');
      });
    }
  }
  els.crumbs.innerHTML = parts.join('');
}

function buildFileCard(file) {
  const meta = [];
  const formattedSize = formatBytes(file.size);
  if (formattedSize) meta.push(formattedSize);
  const timeLabel = formatTime(file);
  if (timeLabel) meta.push(timeLabel);
  const thumb = isImageFile(file.path)
    ? '<div class="thumb"><img src="' + file.cdn_url + '" alt="' + escapeHtml(basename(file.path)) + '" loading="lazy"></div>'
    : '<div class="thumb">' + fileIcon(file.path) + '</div>';
  const encodedPath = encodeURIComponent(file.path);
  const activeClass = state.menuPath === file.path ? ' active' : '';
  return '<article class="file-row' + activeClass + '" data-path="' + encodedPath + '">' +
    thumb +
    '<div class="file-body">' +
      '<div class="file-name">' + escapeHtml(basename(file.path)) + '</div>' +
      '<div class="file-meta"><span>' + escapeHtml(file.path || '') + '</span>' + (meta.length ? '<span>' + escapeHtml(meta.join(' · ')) + '</span>' : '') + '</div>' +
    '</div>' +
    '<div class="file-size">' + escapeHtml(formattedSize) + '</div>' +
    '<button class="more-btn" type="button" data-menu-path="' + encodedPath + '">⋯</button>' +
  '</article>';
}

function renderFiles() {
  if (!state.files.length) {
    els.fileList.innerHTML = '<div class="empty">当前目录没有文件</div>';
    return;
  }
  els.fileList.innerHTML = state.files.map(buildFileCard).join('');
}

async function apiJSON(url, options) {
  const res = await fetch(url, options || {});
  const data = await res.json();
  if (!res.ok || data.success === false) {
    throw new Error(data.error || '请求失败');
  }
  return data;
}

function populateSelect(select, includeRoot) {
  const rootOption = includeRoot ? '<option value="">根目录</option>' : '';
  select.innerHTML = rootOption + state.config.folders.map(v => '<option value="' + v + '">' + v + '</option>').join('');
}

async function loadConfig() {
  const data = await apiJSON('/api/config');
  state.config = data;
  state.buckets = data.buckets || [];
  state.currentBucket = '';
  state.currentFolder = '';
  state.currentSubdir = '';
  state.viewingBuckets = true;

  renderBuckets();
}

function formatBytes(bytes) {
  if (!bytes) return '';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (bytes >= 1024 && i < units.length - 1) {
    bytes /= 1024;
    i++;
  }
  return bytes.toFixed(1) + ' ' + units[i];
}

function renderBuckets() {
  closeActionMenu();
  if (!state.buckets.length) {
    els.fileList.innerHTML = '<div class="empty">暂无可用存储桶</div>';
    els.browserStatus.textContent = '';
    return;
  }
  els.browserStatus.textContent = '我的存储桶 · ' + state.buckets.length + ' 个存储桶';
  els.fileList.innerHTML = state.buckets.map(buildBucketCard).join('');
  renderBreadcrumbs();
}

function buildBucketCard(bucket) {
  const encodedName = encodeURIComponent(bucket.name);
  const size = formatBytes(bucket.size);
  const files = bucket.total_files !== undefined ? bucket.total_files + ' 个文件' : '';
  const modified = bucket.last_modified ? new Date(bucket.last_modified).toLocaleDateString('zh-CN') : '';
  const meta = [size, files, modified].filter(Boolean).join(' · ');
  const activeClass = state.menuPath === bucket.name ? ' active' : '';
  return '<article class="file-row' + activeClass + '" data-bucket="' + encodedName + '">' +
    '<div class="thumb">📦</div>' +
    '<div class="file-body">' +
      '<div class="file-name">' + escapeHtml(bucket.name) + '</div>' +
      '<div class="file-meta"><span>' + escapeHtml(bucket.name) + '</span>' + (meta ? '<span>' + escapeHtml(meta) + '</span>' : '') + '</div>' +
    '</div>' +
    '<button class="more-btn" type="button" data-bucket-name="' + encodedName + '">⋯</button>' +
  '</article>';
}

function showFileList(bucketName) {
  state.viewingBuckets = false;
  state.currentBucket = bucketName;
  state.currentFolder = '';
  state.currentSubdir = '';
  els.backToBuckets.style.display = '';
  refreshFiles();
}

function showBucketList() {
  state.viewingBuckets = true;
  state.currentBucket = '';
  state.currentFolder = '';
  state.currentSubdir = '';
  els.backToBuckets.style.display = 'none';
  els.browseBucket.style.display = 'none';
  renderBuckets();
}

async function refreshFiles() {
  closeActionMenu();
  els.browserStatus.textContent = '正在刷新文件列表...';
  els.fileList.innerHTML = '';
  const params = new URLSearchParams({bucket: state.currentBucket, folder: state.currentFolder, subdir: state.currentSubdir});
  const data = await apiJSON('/api/files?' + params.toString());
  state.files = data.files || [];
  state.fileMap = new Map(state.files.map(file => [file.path, file]));
  state.currentFolder = data.folder || '';
  renderBreadcrumbs();
  const label = state.currentFolder || '根目录';
  els.browserStatus.textContent = '当前目录 ' + label + ' · ' + state.files.length + ' 个文件';
  renderFiles();
}

function openActionMenu(button, path) {
  state.menuPath = path;
  renderFiles();
  const rect = button.getBoundingClientRect();
  els.actionMenu.style.top = Math.min(window.innerHeight - 20, rect.top + 10) + 'px';
  els.actionMenu.style.left = Math.max(16, rect.left - 220) + 'px';
  els.actionMenu.classList.add('open');
  els.actionMenu.setAttribute('aria-hidden', 'false');
}

function closeActionMenu() {
  state.menuPath = '';
  els.actionMenu.classList.remove('open');
  els.actionMenu.setAttribute('aria-hidden', 'true');
}

function openOverlay(overlay) {
  overlay.classList.add('open');
  overlay.setAttribute('aria-hidden', 'false');
}

function closeOverlay(overlay) {
  overlay.classList.remove('open');
  overlay.setAttribute('aria-hidden', 'true');
}

function fieldHTML(label, id, value, extra) {
  return '<div class="field"><label for="' + id + '">' + label + '</label><input id="' + id + '" value="' + escapeHtml(value || '') + '" ' + (extra || '') + '></div>';
}

function selectHTML(label, id, selected) {
  const options = ['<option value="">根目录</option>'].concat(state.config.folders.map(function (folder) {
    const sel = folder === selected ? ' selected' : '';
    return '<option value="' + folder + '"' + sel + '>' + folder + '</option>';
  }));
  return '<div class="field"><label for="' + id + '">' + label + '</label><select id="' + id + '">' + options.join('') + '</select></div>';
}

function openDialog(type, file) {
  state.actionType = type;
  state.actionFile = file;
  const name = basename(file.path);
  const dir = dirname(file.path);
  let title = '';
  let desc = '';
  let html = '';
  let submit = '确认';

  if (type === 'delete') {
    title = '删除文件';
    desc = '删除前请再次输入文件名确认。这个操作不可撤销。';
    html = fieldHTML('确认文件名', 'dialog-confirm-name', '', 'placeholder="输入 ' + escapeHtml(name) + '"');
    submit = '删除';
  } else if (type === 'rename') {
    title = '重命名';
    desc = '输入当前文件名进行二次确认，再填写新的文件名。';
    html = fieldHTML('确认文件名', 'dialog-confirm-name', '', 'placeholder="输入 ' + escapeHtml(name) + '"') + fieldHTML('新文件名', 'dialog-target-name', name, 'required');
    submit = '保存';
  } else if (type === 'move') {
    title = '移动文件';
    desc = '输入当前文件名确认后，选择目标目录。复制成功后会删除原文件。';
    html = fieldHTML('确认文件名', 'dialog-confirm-name', '', 'placeholder="输入 ' + escapeHtml(name) + '"') + selectHTML('目标顶层目录', 'dialog-target-folder', dir.split('/')[0] || '') + fieldHTML('目标子目录', 'dialog-target-subdir', dir.split('/').slice(1).join('/'), 'placeholder="可留空"') + fieldHTML('目标文件名', 'dialog-target-name', name, 'required');
    submit = '移动';
  } else if (type === 'copy') {
    title = '复制文件';
    desc = '复制到新目录或新文件名，原文件会保留。';
    html = selectHTML('目标顶层目录', 'dialog-target-folder', dir.split('/')[0] || '') + fieldHTML('目标子目录', 'dialog-target-subdir', dir.split('/').slice(1).join('/'), 'placeholder="可留空"') + fieldHTML('目标文件名', 'dialog-target-name', name, 'required');
    submit = '复制';
  }

  els.dialogTitle.textContent = title;
  els.dialogDescription.textContent = desc;
  els.dialogFields.innerHTML = html;
  els.dialogSubmit.textContent = submit;
  openOverlay(els.dialogOverlay);
}

async function submitDialogAction() {
  const file = state.actionFile;
  const type = state.actionType;
  if (!file || !type) return;
  const payload = {bucket: state.currentBucket, path: file.path};
  if (type === 'delete' || type === 'rename' || type === 'move') {
    payload.confirm_name = document.getElementById('dialog-confirm-name').value.trim();
  }
  if (type === 'rename') {
    payload.target_name = document.getElementById('dialog-target-name').value.trim();
  }
  if (type === 'move' || type === 'copy') {
    payload.target_folder = document.getElementById('dialog-target-folder').value;
    payload.target_subdir = document.getElementById('dialog-target-subdir').value.trim();
    payload.target_name = document.getElementById('dialog-target-name').value.trim();
  }
  const endpointMap = {
    delete: '/api/file/delete',
    rename: '/api/file/rename',
    move: '/api/file/move',
    copy: '/api/file/copy'
  };
  els.dialogSubmit.disabled = true;
  try {
    const result = await apiJSON(endpointMap[type], {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(payload)});
    closeOverlay(els.dialogOverlay);
    if (result.new_path) {
      const newDir = dirname(result.new_path);
      const parts = newDir ? newDir.split('/') : [];
      state.currentFolder = parts[0] || '';
      state.currentSubdir = parts.slice(1).join('/');
    }
    await refreshFiles();
  } catch (error) {
    els.dialogDescription.textContent = error.message;
  } finally {
    els.dialogSubmit.disabled = false;
  }
}

async function copyText(text, label) {
  await navigator.clipboard.writeText(text);
  els.browserStatus.textContent = label + ' 已复制';
}

async function handleMenuAction(action) {
  const file = state.fileMap.get(state.menuPath);
  closeActionMenu();
  if (!file) return;
  if (action === 'copy-cdn') {
    await copyText(file.cdn_url, 'CDN 链接');
    return;
  }
  if (action === 'copy-direct') {
    await copyText(file.direct_url, '直链');
    return;
  }
  if (action === 'download') {
    window.open(file.direct_url, '_blank', 'noopener,noreferrer');
    return;
  }
  if (action === 'share') {
    if (navigator.share) {
      try {
        await navigator.share({title: basename(file.path), url: file.cdn_url});
      } catch (_) {}
    } else {
      await copyText(file.cdn_url, '分享链接');
    }
    return;
  }
  openDialog(action, file);
}

function openUploadModal() {
  clearUploadResult();
  els.uploadStatus.textContent = '';
  els.uploadBucket.value = state.currentBucket;
  els.uploadFolder.value = state.currentFolder ? state.currentFolder.split('/')[0] : '';
  els.uploadSubdir.value = state.currentFolder ? state.currentFolder.split('/').slice(1).join('/') : '';
  openOverlay(els.uploadOverlay);
}

els.openUpload.addEventListener('click', openUploadModal);
els.uploadCancel.addEventListener('click', function () { closeOverlay(els.uploadOverlay); });
els.dialogCancel.addEventListener('click', function () { closeOverlay(els.dialogOverlay); });
els.dialogSubmit.addEventListener('click', submitDialogAction);
els.refreshFiles.addEventListener('click', async function () {
  try {
    await refreshFiles();
  } catch (error) {
    els.browserStatus.textContent = error.message;
    els.fileList.innerHTML = '<div class="empty">' + escapeHtml(error.message) + '</div>';
  }
});
els.viewButtons.forEach(function (btn) { btn.addEventListener('click', function () { setView(btn.dataset.view); }); });
els.backToBuckets.addEventListener('click', showBucketList);

document.addEventListener('click', function (event) {
  const bucketRow = event.target.closest('.file-row[data-bucket]');
  if (bucketRow) {
    const bucketName = decodeURIComponent(bucketRow.dataset.bucket);
    showFileList(bucketName);
    return;
  }
  const crumbLink = event.target.closest('.crumb-link[data-crumb]');
  if (crumbLink && crumbLink.dataset.crumb === 'buckets') {
    showBucketList();
    return;
  }
  const menuBtn = event.target.closest('[data-menu-path]');
  if (menuBtn) {
    const path = decodeURIComponent(menuBtn.dataset.menuPath);
    if (state.menuPath === path && els.actionMenu.classList.contains('open')) {
      closeActionMenu();
      renderFiles();
      return;
    }
    openActionMenu(menuBtn, path);
    return;
  }
  if (!event.target.closest('#action-menu')) {
    closeActionMenu();
    renderFiles();
  }
});

els.actionMenu.addEventListener('click', function (event) {
  const button = event.target.closest('button[data-action]');
  if (!button) return;
  handleMenuAction(button.dataset.action).catch(function (error) {
    els.browserStatus.textContent = error.message;
  });
});

els.uploadForm.addEventListener('submit', async function (event) {
  event.preventDefault();
  clearUploadResult();
  els.uploadStatus.textContent = '正在上传...';
  const formData = new FormData(els.uploadForm);
  try {
    const res = await fetch('/api/upload', {method: 'POST', body: formData});
    const data = await res.json();
    if (!res.ok || !data.success) throw new Error(data.error || '上传失败');
    els.uploadStatus.textContent = '上传完成';
    showUploadResult('ok', '<h3>上传完成</h3><div><strong>路径：</strong>' + escapeHtml((data.folder ? data.folder + '/' : '') + data.file_name) + '</div><div style="margin-top:10px"><strong>CDN：</strong><br><a href="' + data.cdn_url + '" target="_blank" rel="noreferrer">' + data.cdn_url + '</a></div><div style="margin-top:10px"><strong>直链：</strong><br><a href="' + data.direct_url + '" target="_blank" rel="noreferrer">' + data.direct_url + '</a></div>');
    state.currentBucket = data.bucket;
    state.currentFolder = data.folder || '';
    state.currentSubdir = '';
    await refreshFiles();
  } catch (error) {
    els.uploadStatus.textContent = '上传失败';
    showUploadResult('err', '<h3>上传失败</h3><div>' + escapeHtml(error.message) + '</div>');
  }
});

loadConfig().then(function () { setView('list'); }).catch(function (error) {
  els.browserStatus.textContent = '页面初始化失败';
  els.fileList.innerHTML = '<div class="empty">' + escapeHtml(error.message) + '</div>';
});
</script>
</body>
</html>`

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	state, err := newStateStore(cfg.StateFile, cfg.StateFlushInterval)
	if err != nil {
		log.Fatalf("Failed to load state: %v", err)
	}
	defer state.Close()

	bot, err := newBot(cfg, state)
	if err != nil {
		log.Fatalf("Failed to initialize bot: %v", err)
	}

	if cfg.WebEnabled {
		webServer := bot.newWebServer()
		go func() {
			log.Printf("Web upload UI listening on %s", cfg.WebListenAddr)
			if err := webServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("Web server failed: %v", err)
			}
		}()
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
		if len(updates) > 0 {
			if err := state.Flush(); err != nil {
				log.Printf("Failed to flush state after update batch: %v", err)
			}
		}

		if len(updates) == 0 {
			time.Sleep(1 * time.Second)
		}
	}
}
