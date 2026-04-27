package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var apiClient = &http.Client{Timeout: 15 * time.Second}
var downloadClient = &http.Client{Timeout: 10 * time.Minute}

var defaultFolders = []string{"images", "videos", "documents", "others"}

const defaultStateFile = "./data/state.json"

type Config struct {
	BotToken             string
	HFToken              string
	HFRepoID             string
	CDNBaseURL           string
	Folders              []string
	StateFile            string
	AllowedUserIDs       map[int64]struct{}
	AllowedChatIDs       map[int64]struct{}
	MaxConcurrentUploads int
	UploadQueueCapacity  int
	StateFlushInterval   time.Duration
}

type Bot struct {
	config      Config
	state       *StateStore
	uploadQueue chan UploadJob
}

type UserStats struct {
	Total   int `json:"total"`
	Success int `json:"success"`
	Fail    int `json:"fail"`
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
	ChatID      int64
	UserID      int64
	FileName    string
	Folder      string
	DownloadURL string
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

type sendMessageResponse struct {
	telegramBaseResponse
	Result struct {
		MessageID int64 `json:"message_id"`
	} `json:"result"`
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

func loadConfig() (Config, error) {
	cfg := Config{
		BotToken:             strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		HFToken:              strings.TrimSpace(os.Getenv("HF_TOKEN")),
		HFRepoID:             strings.TrimSpace(os.Getenv("HF_REPO_ID")),
		CDNBaseURL:           strings.TrimRight(strings.TrimSpace(os.Getenv("CDN_BASE_URL")), "/"),
		Folders:              parseListEnv(os.Getenv("HF_FOLDERS"), defaultFolders),
		StateFile:            strings.TrimSpace(os.Getenv("STATE_FILE")),
		AllowedUserIDs:       parseInt64SetEnv(os.Getenv("TELEGRAM_ALLOWED_USER_IDS")),
		AllowedChatIDs:       parseInt64SetEnv(os.Getenv("TELEGRAM_ALLOWED_CHAT_IDS")),
		MaxConcurrentUploads: parsePositiveIntEnv(os.Getenv("MAX_CONCURRENT_UPLOADS"), 2),
		StateFlushInterval:   parseDurationSecondsEnv(os.Getenv("STATE_FLUSH_INTERVAL_SECONDS"), 2*time.Second),
	}

	if cfg.BotToken == "" {
		return Config{}, errors.New("TELEGRAM_BOT_TOKEN not set")
	}
	if cfg.HFToken == "" {
		return Config{}, errors.New("HF_TOKEN not set")
	}
	if cfg.HFRepoID == "" {
		return Config{}, errors.New("HF_REPO_ID not set")
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
	return cfg, nil
}

func parseListEnv(raw string, fallback []string) []string {
	if strings.TrimSpace(raw) == "" {
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
	if len(result) == 0 {
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

func (s *StateStore) GetUserStats(userID int64) UserStats {
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

func newBot(cfg Config, state *StateStore) *Bot {
	if len(cfg.AllowedUserIDs) == 0 && len(cfg.AllowedChatIDs) == 0 {
		log.Printf("Warning: no Telegram allowlist configured; the bot will accept updates from any user or chat")
	}
	log.Printf("Runtime config: repo=%s folders=%v state=%s max_concurrent_uploads=%d", cfg.HFRepoID, cfg.Folders, cfg.StateFile, cfg.MaxConcurrentUploads)
	bot := &Bot{
		config:      cfg,
		state:       state,
		uploadQueue: make(chan UploadJob, cfg.UploadQueueCapacity),
	}
	for i := 0; i < cfg.MaxConcurrentUploads; i++ {
		go bot.uploadWorker()
	}
	return bot
}

func (b *Bot) isAuthorized(chatID, userID int64) bool {
	if len(b.config.AllowedUserIDs) == 0 && len(b.config.AllowedChatIDs) == 0 {
		return true
	}
	_, userOK := b.config.AllowedUserIDs[userID]
	_, chatOK := b.config.AllowedChatIDs[chatID]
	return userOK || chatOK
}

func (b *Bot) uploadWorker() {
	for job := range b.uploadQueue {
		b.processFile(job)
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

func (b *Bot) sendMessage(chatID int64, text string) (int64, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", b.config.BotToken)
	payload, err := json.Marshal(map[string]interface{}{"chat_id": chatID, "text": text})
	if err != nil {
		return 0, err
	}
	resp, err := apiClient.Post(url, "application/json", bytes.NewBuffer(payload))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var result sendMessageResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}
	if !result.OK {
		return 0, fmt.Errorf("sendMessage failed: %s", result.Description)
	}
	return result.Result.MessageID, nil
}

func (b *Bot) editMessageText(chatID, messageID int64, text string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/editMessageText", b.config.BotToken)
	payload, err := json.Marshal(map[string]interface{}{"chat_id": chatID, "message_id": messageID, "text": text})
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
		return fmt.Errorf("editMessageText failed: %s", result.Description)
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

func sanitizePathComponent(value string) string {
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r == 0 || r < 32:
			builder.WriteByte('_')
		case strings.ContainsRune(`/\\?#%*:|"<>`, r):
			builder.WriteByte('_')
		default:
			builder.WriteRune(r)
		}
	}
	return strings.TrimSpace(builder.String())
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
	if safe == "" {
		return "file"
	}
	return safe
}

func encodePathSegments(parts ...string) string {
	encoded := make([]string, 0, len(parts))
	for _, part := range parts {
		for _, segment := range strings.Split(part, "/") {
			if segment == "" {
				continue
			}
			encoded = append(encoded, url.PathEscape(segment))
		}
	}
	return strings.Join(encoded, "/")
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

func (b *Bot) uploadToHF(filePath, fileName, folder string) (string, error) {
	pathInRepo := encodePathSegments(folder, fileName)
	uploadURL := fmt.Sprintf("https://huggingface.co/datasets/%s/upload/main/%s", b.config.HFRepoID, pathInRepo)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		return "", err
	}
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(part, f); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPost, uploadURL, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+b.config.HFToken)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("HF API error: %d - %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	if b.config.CDNBaseURL != "" {
		return fmt.Sprintf("%s/%s", b.config.CDNBaseURL, pathInRepo), nil
	}
	return fmt.Sprintf("https://huggingface.co/datasets/%s/resolve/main/%s", b.config.HFRepoID, pathInRepo), nil
}

func (b *Bot) processFile(job UploadJob) {
	chatID := job.ChatID
	userID := job.UserID
	fileName := job.FileName
	folder := job.Folder
	downloadURL := job.DownloadURL

	if err := b.state.RecordAttempt(userID, defaultFolder(b.config.Folders)); err != nil {
		log.Printf("Failed to persist attempt counter: %v", err)
	}

	messageID, err := b.sendMessage(chatID, "⏳ 正在处理: "+fileName+"...")
	if err != nil {
		log.Printf("Failed to send processing message: %v", err)
	}

	tmpDir := "/tmp/hf_uploads"
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		b.handleUploadFailure(chatID, userID, messageID, "❌ 创建临时目录失败: "+err.Error())
		return
	}
	localName := fmt.Sprintf("%d_%d_%s", userID, time.Now().UnixNano(), fileName)
	localPath := filepath.Join(tmpDir, localName)

	resp, err := downloadClient.Get(downloadURL)
	if err != nil {
		b.handleUploadFailure(chatID, userID, messageID, "❌ 下载失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		b.handleUploadFailure(chatID, userID, messageID, fmt.Sprintf("❌ 下载失败，状态码 %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
		return
	}

	out, err := os.Create(localPath)
	if err != nil {
		b.handleUploadFailure(chatID, userID, messageID, "❌ 创建文件失败: "+err.Error())
		return
	}
	_, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		os.Remove(localPath)
		b.handleUploadFailure(chatID, userID, messageID, "❌ 下载失败: "+copyErr.Error())
		return
	}
	if closeErr != nil {
		os.Remove(localPath)
		b.handleUploadFailure(chatID, userID, messageID, "❌ 写入临时文件失败: "+closeErr.Error())
		return
	}
	defer os.Remove(localPath)

	if messageID != 0 {
		if err := b.editMessageText(chatID, messageID, "⏳ 上传到 "+folder+"/"+fileName+"..."); err != nil {
			log.Printf("Failed to update progress message: %v", err)
		}
	}

	cdnURL, err := b.uploadToHF(localPath, fileName, folder)
	if err != nil {
		b.handleUploadFailure(chatID, userID, messageID, "❌ 上传失败: "+err.Error())
		return
	}
	if err := b.state.RecordSuccess(userID, defaultFolder(b.config.Folders)); err != nil {
		log.Printf("Failed to persist success counter: %v", err)
	}

	text := fmt.Sprintf("✅ 上传成功！\n\n📁 文件夹: %s\n🔗 %s", folder, cdnURL)
	if messageID != 0 {
		if err := b.editMessageText(chatID, messageID, text); err == nil {
			return
		}
	}
	if _, err := b.sendMessage(chatID, text); err != nil {
		log.Printf("Failed to send success message: %v", err)
	}
}

func (b *Bot) handleUploadFailure(chatID, userID, messageID int64, text string) {
	if err := b.state.RecordFailure(userID, defaultFolder(b.config.Folders)); err != nil {
		log.Printf("Failed to persist failure counter: %v", err)
	}
	if messageID != 0 {
		if err := b.editMessageText(chatID, messageID, text); err == nil {
			return
		}
	}
	if _, err := b.sendMessage(chatID, text); err != nil {
		log.Printf("Failed to send failure message: %v", err)
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

	if !b.isAuthorized(chatID, userID) {
		log.Printf("Ignoring unauthorized update from chat=%d user=%d", chatID, userID)
		return
	}
	if err := b.state.EnsureUser(userID, defaultUserFolder); err != nil {
		log.Printf("Failed to persist user state: %v", err)
	}

	command := parseCommand(msg.Text)
	if command != "" {
		switch command {
		case "/start":
			_, _ = b.sendMessage(chatID, "👋 欢迎！我是 Hugging Face 上传 Bot\n\n发送文件直接上传\n/folder - 切换文件夹\n/folders - 查看所有文件夹")
			return
		case "/help":
			_, _ = b.sendMessage(chatID, "📖 使用帮助\n\n发送文件（图片、视频、音频、文档）我会自动上传到 Hugging Face\n\n📁 文件夹："+strings.Join(b.config.Folders, ", "))
			return
		case "/folder":
			_, _ = b.sendMessage(chatID, fmt.Sprintf("📁 当前文件夹: %s\n可选: %s\n\n回复文件夹名切换", b.state.GetUserFolder(userID, defaultUserFolder), strings.Join(b.config.Folders, ", ")))
			return
		case "/folders":
			currentFolder := b.state.GetUserFolder(userID, defaultUserFolder)
			result := "📂 可用文件夹：\n"
			for _, folder := range b.config.Folders {
				mark := ""
				if folder == currentFolder {
					mark = " ✓"
				}
				result += "• " + folder + mark + "\n"
			}
			_, _ = b.sendMessage(chatID, strings.TrimSpace(result))
			return
		case "/status":
			cdnValue := b.config.CDNBaseURL
			if cdnValue == "" {
				cdnValue = "未配置"
			}
			_, _ = b.sendMessage(chatID, fmt.Sprintf("⚙️ 当前状态\n\n📁 文件夹: %s\n📂 可用: %s\n🔗 CDN: %s\n📦 仓库: %s\n🚦 并发上传上限: %d", b.state.GetUserFolder(userID, defaultUserFolder), strings.Join(b.config.Folders, ", "), cdnValue, b.config.HFRepoID, b.config.MaxConcurrentUploads))
			return
		case "/stats":
			stats := b.state.GetUserStats(userID)
			rate := 0.0
			if stats.Total > 0 {
				rate = float64(stats.Success) / float64(stats.Total) * 100
			}
			_, _ = b.sendMessage(chatID, fmt.Sprintf("📊 上传统计\n\n总计: %d\n成功: %d\n失败: %d\n成功率: %.1f%%", stats.Total, stats.Success, stats.Fail, rate))
			return
		}
	}

	if msg.Text != "" && !strings.HasPrefix(strings.TrimSpace(msg.Text), "/") {
		text := strings.ToLower(strings.TrimSpace(msg.Text))
		if b.setUserFolder(userID, text) {
			_, _ = b.sendMessage(chatID, "✅ 已切换到文件夹: "+text)
		}
		return
	}

	folder := b.state.GetUserFolder(userID, defaultUserFolder)
	if msg.Document != nil {
		doc := msg.Document
		downloadURL, err := b.getFile(doc.FileID)
		if err != nil {
			_, _ = b.sendMessage(chatID, "❌ 获取文件地址失败: "+err.Error())
			return
		}
		fileName := sanitizeFileName(doc.FileName, doc.FileUniqueID)
		job := UploadJob{ChatID: chatID, UserID: userID, FileName: fileName, Folder: folder, DownloadURL: downloadURL}
		if !b.enqueueUpload(job) {
			_, _ = b.sendMessage(chatID, "❌ 上传队列已满，请稍后再试")
			return
		}
		_, _ = b.sendMessage(chatID, "🕓 已加入上传队列: "+fileName)
		return
	}
	if len(msg.Photo) > 0 {
		photo := msg.Photo[len(msg.Photo)-1]
		downloadURL, err := b.getFile(photo.FileID)
		if err != nil {
			_, _ = b.sendMessage(chatID, "❌ 获取图片地址失败: "+err.Error())
			return
		}
		fileName := sanitizeFileName(photo.FileUniqueID+".jpg", photo.FileUniqueID+".jpg")
		job := UploadJob{ChatID: chatID, UserID: userID, FileName: fileName, Folder: folder, DownloadURL: downloadURL}
		if !b.enqueueUpload(job) {
			_, _ = b.sendMessage(chatID, "❌ 上传队列已满，请稍后再试")
			return
		}
		_, _ = b.sendMessage(chatID, "🕓 已加入上传队列: "+fileName)
		return
	}
	if msg.Video != nil {
		video := msg.Video
		downloadURL, err := b.getFile(video.FileID)
		if err != nil {
			_, _ = b.sendMessage(chatID, "❌ 获取视频地址失败: "+err.Error())
			return
		}
		fallbackName := "video_" + video.FileUniqueID + ".mp4"
		fileName := sanitizeFileName(video.FileName, fallbackName)
		job := UploadJob{ChatID: chatID, UserID: userID, FileName: fileName, Folder: folder, DownloadURL: downloadURL}
		if !b.enqueueUpload(job) {
			_, _ = b.sendMessage(chatID, "❌ 上传队列已满，请稍后再试")
			return
		}
		_, _ = b.sendMessage(chatID, "🕓 已加入上传队列: "+fileName)
		return
	}
	if msg.Audio != nil {
		audio := msg.Audio
		downloadURL, err := b.getFile(audio.FileID)
		if err != nil {
			_, _ = b.sendMessage(chatID, "❌ 获取音频地址失败: "+err.Error())
			return
		}
		fallbackName := "audio_" + audio.FileUniqueID + ".mp3"
		fileName := sanitizeFileName(audio.FileName, fallbackName)
		job := UploadJob{ChatID: chatID, UserID: userID, FileName: fileName, Folder: folder, DownloadURL: downloadURL}
		if !b.enqueueUpload(job) {
			_, _ = b.sendMessage(chatID, "❌ 上传队列已满，请稍后再试")
			return
		}
		_, _ = b.sendMessage(chatID, "🕓 已加入上传队列: "+fileName)
		return
	}
}

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
	bot := newBot(cfg, state)

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
