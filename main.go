package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var (
	botToken = os.Getenv("TELEGRAM_BOT_TOKEN")
	hfToken = os.Getenv("HF_TOKEN")
	cdnBaseURL = os.Getenv("CDN_BASE_URL")
	hfFolders = os.Getenv("HF_FOLDERS")
)

var defaultFolders = []string{"images", "videos", "documents", "others"}
var userFolders = make(map[int64]string)
var userStats = make(map[int64]*UserStats)
var availableBuckets []string
var hfUsername string

func init() {
	username := fetchUsername()
	if username == "" {
		username = "anyaer007"
	}
	hfUsername = username
	availableBuckets = fetchBuckets(username)
	if len(availableBuckets) == 0 {
		availableBuckets = []string{"image"}
	}
	log.Printf("Available buckets: %v, username: %s", availableBuckets, hfUsername)
}

func fetchUsername() string {
	cmd := exec.Command("python3", "-c", "from huggingface_hub import HfApi; print(HfApi().whoami()['name'])")
	cmd.Env = append(os.Environ(), "HF_TOKEN="+hfToken)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Failed to fetch username: %v, output: %s", err, string(output))
		return ""
	}
	return strings.TrimSpace(string(output))
}

func fetchBuckets(username string) []string {
	url := fmt.Sprintf("https://huggingface.co/api/buckets/%s", username)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Printf("Failed to create request: %v", err)
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+hfToken)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Failed to fetch buckets: %v", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("Failed to fetch buckets, status: %d", resp.StatusCode)
		return nil
	}

	var buckets []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&buckets); err != nil {
		log.Printf("Failed to decode buckets: %v", err)
		return nil
	}

	result := make([]string, 0, len(buckets))
	for _, b := range buckets {
		if id, ok := b["id"].(string); ok {
			parts := strings.Split(id, "/")
			if len(parts) == 2 {
				result = append(result, parts[1])
			}
		}
	}
	return result
}

type UserStats struct {
	Total int `json:"total"`
	Success int `json:"success"`
	Fail int `json:"fail"`
	PreferredBucket string `json:"preferred_bucket"`
}

type Update struct {
	UpdateID int64  `json:"update_id"`
	Message  *Message `json:"message,omitempty"`
}

type Message struct {
	MessageID int64  `json:"message_id"`
	Chat      Chat   `json:"chat"`
	From      *User  `json:"from,omitempty"`
	Text      string `json:"text,omitempty"`
	Document  *Document `json:"document,omitempty"`
	Photo     []PhotoSize `json:"photo,omitempty"`
	Video     *Video `json:"video,omitempty"`
	Audio     *Audio `json:"audio,omitempty"`
	Entities  []MessageEntity `json:"entities,omitempty"`
}

type Chat struct {
	ID int64 `json:"id"`
}

type User struct {
	ID int64 `json:"id"`
}

type Document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
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

type MessageEntity struct {
	Type string `json:"type"`
	Off int    `json:"offset"`
	Len int    `json:"length"`
}

type FileResponse struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FilePath     string `json:"file_path"`
}

type GetUpdatesResponse struct {
	OK     bool     `json:"ok"`
	Result []Update `json:"result"`
}

type GetFileResponse struct {
	OK     bool `json:"ok"`
	Result FileResponse `json:"result"`
}

func getFolders() []string {
	if hfFolders != "" {
		parts := strings.Split(hfFolders, ",")
		var result []string
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				result = append(result, p)
			}
		}
		if len(result) > 0 {
			return result
		}
	}
	return defaultFolders
}

func getUserFolder(userID int64) string {
	if folder, ok := userFolders[userID]; ok {
		return folder
	}
	return "others"
}

func setUserFolder(userID int64, folder string) bool {
	folders := getFolders()
	for _, f := range folders {
		if f == folder {
			userFolders[userID] = folder
			return true
		}
	}
	return false
}

func setUserBucket(userID int64, bucket string) bool {
	for _, b := range availableBuckets {
		if b == bucket {
			if _, ok := userStats[userID]; !ok {
				userStats[userID] = &UserStats{}
			}
			userStats[userID].PreferredBucket = bucket
			return true
		}
	}
	return false
}

func getMe() (string, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getMe", botToken)
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	json.Unmarshal(body, &result)
	if result["ok"] == true {
		m := result["result"].(map[string]interface{})
		return m["username"].(string), nil
	}
	return "", fmt.Errorf("getMe failed: %s", body)
}

func setMyCommands() error {
	commands := []map[string]string{
		{"command": "start", "description": "启动 Bot"},
		{"command": "help", "description": "帮助信息"},
		{"command": "folder", "description": "切换文件夹"},
		{"command": "folders", "description": "查看所有文件夹"},
		{"command": "bucket", "description": "切换存储桶"},
		{"command": "buckets", "description": "查看所有存储桶"},
		{"command": "status", "description": "查看状态"},
		{"command": "stats", "description": "查看统计"},
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/setMyCommands", botToken)
	bodyBytes, _ := json.Marshal(map[string]interface{}{"commands": commands})
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("setMyCommands failed with status: %d", resp.StatusCode)
	}
	return nil
}

func getUpdates(offset int64) ([]Update, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=10", botToken, offset)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result GetUpdatesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Result, nil
}

func getFile(fileID string) (string, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getFile?file_id=%s", botToken, fileID)
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result GetFileResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", botToken, result.Result.FilePath), nil
}

func sendMessage(chatID int64, text string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
	data := map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	}
	body, _ := json.Marshal(data)
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func editMessageText(chatID int64, messageID int64, text string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/editMessageText", botToken)
	data := map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
	}
	body, _ := json.Marshal(data)
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func getCommand(text string, entities []MessageEntity) string {
	if entities == nil || len(entities) == 0 {
		return ""
	}
	for _, e := range entities {
		if e.Type == "bot_command" {
			if e.Off < len(text) && e.Len > 0 && e.Off+e.Len <= len(text) {
				return text[e.Off:e.Off+e.Len]
			}
		}
	}
	return ""
}

func uploadToHF(filePath, fileName, folder, bucket string) (string, error) {
	repoID := fmt.Sprintf("%s/%s", hfUsername, bucket)
	dstPath := fmt.Sprintf("hf://buckets/%s/%s/%s", repoID, folder, fileName)

	cmd := exec.Command("hf", "buckets", "cp", filePath, dstPath)
	cmd.Env = append(os.Environ(), "HF_TOKEN="+hfToken)

	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("HF CLI error: %s, output: %s", err, string(output))
		return "", fmt.Errorf("HF CLI error: %s - %s", err, string(output))
	}

	log.Printf("HF upload success: %s", string(output))

	if cdnBaseURL != "" {
		return fmt.Sprintf("%s/%s/%s", cdnBaseURL, folder, fileName), nil
	}
	return fmt.Sprintf("https://huggingface.co/buckets/%s/resolve/%s/%s", repoID, folder, fileName), nil
}

func getUserBucket(userID int64) string {
	if stats, ok := userStats[userID]; ok && stats.PreferredBucket != "" {
		return stats.PreferredBucket
	}
	if len(availableBuckets) > 0 {
		return availableBuckets[0]
	}
	return "image"
}

func processFile(chatID int64, fileID, fileName, folder, bucket, downloadURL string) {
	log.Printf("Processing file: %s bucket=%s folder=%s from %s", fileName, bucket, folder, downloadURL)

	if _, ok := userStats[chatID]; !ok {
		userStats[chatID] = &UserStats{}
	}
	userStats[chatID].Total++

	sendMessage(chatID, "⏳ 正在处理: "+fileName+"...")

	tmpDir := "/tmp/hf_uploads"
	os.MkdirAll(tmpDir, 0755)
	localPath := filepath.Join(tmpDir, fileName)

	resp, err := http.Get(downloadURL)
	if err != nil {
		log.Printf("Download failed: %v", err)
		sendMessage(chatID, "❌ 下载失败: "+err.Error())
		userStats[chatID].Fail++
		return
	}
	defer resp.Body.Close()

	out, err := os.Create(localPath)
	if err != nil {
		log.Printf("Create file failed: %v", err)
		sendMessage(chatID, "❌ 创建文件失败: "+err.Error())
		userStats[chatID].Fail++
		return
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		log.Printf("Copy failed: %v", err)
		sendMessage(chatID, "❌ 下载失败: "+err.Error())
		userStats[chatID].Fail++
		return
	}

	sendMessage(chatID, "⏳ 上传到 "+bucket+"/"+folder+"/"+fileName+"...")

	cdnURL, err := uploadToHF(localPath, fileName, folder, bucket)
	if err != nil {
		log.Printf("Upload failed: %v", err)
		sendMessage(chatID, "❌ 上传失败: "+err.Error())
		userStats[chatID].Fail++
		os.Remove(localPath)
		return
	}

	os.Remove(localPath)
	userStats[chatID].Success++

	sendMessage(chatID, fmt.Sprintf("✅ 上传成功！\n\n📦 存储桶: %s\n📁 文件夹: %s\n🔗 %s", bucket, folder, cdnURL))
}

func handleUpdate(update Update) {
	if update.Message == nil {
		return
	}

	msg := update.Message
	chatID := msg.Chat.ID
	userID := int64(0)
	if msg.From != nil {
		userID = msg.From.ID
	}

	if _, ok := userStats[userID]; !ok {
		userStats[userID] = &UserStats{}
	}
	if _, ok := userFolders[userID]; !ok {
		userFolders[userID] = "others"
	}

	command := getCommand(msg.Text, msg.Entities)
	if command != "" {
		parts := strings.Split(command, "@")
		command = parts[0]

switch command {
		case "/start":
			sendMessage(chatID, "👋 欢迎！我是 Hugging Face 上传 Bot\n\n发送文件直接上传\n/folder - 切换文件夹\n/bucket - 切换存储桶\n/folders - 查看所有文件夹\n/buckets - 查看所有存储桶")
			return
		case "/help":
			sendMessage(chatID, "📖 使用帮助\n\n发送文件（图片、视频、音频、文档）我会自动上传到 Hugging Face\n\n📦 存储桶：image, nixeu\n📁 文件夹：images, videos, documents, others")
			return
		case "/folder":
			folders := getFolders()
			sendMessage(chatID, fmt.Sprintf("📁 当前文件夹: %s\n可选: %s\n\n回复文件夹名切换", getUserFolder(userID), strings.Join(folders, ", ")))
			return
		case "/folders":
			folders := getFolders()
			folder := getUserFolder(userID)
			result := "📂 可用文件夹：\n"
			for _, f := range folders {
				mark := ""
				if f == folder {
					mark = " ✓"
				}
				result += "• " + f + mark + "\n"
			}
			sendMessage(chatID, result)
			return
		case "/bucket":
			sendMessage(chatID, fmt.Sprintf("📦 当前存储桶: %s\n可选: %s\n\n回复存储桶名切换", getUserBucket(userID), strings.Join(availableBuckets, ", ")))
			return
		case "/buckets":
			bucket := getUserBucket(userID)
			result := "📦 可用存储桶：\n"
			for _, b := range availableBuckets {
				mark := ""
				if b == bucket {
					mark = " ✓"
				}
				result += "• " + b + mark + "\n"
			}
			sendMessage(chatID, result)
			return
		case "/status":
			folders := getFolders()
			sendMessage(chatID, fmt.Sprintf("⚙️ 当前状态\n\n📦 存储桶: %s\n📁 文件夹: %s\n📂 可用: %s\n🔗 CDN: %s", getUserBucket(userID), getUserFolder(userID), strings.Join(folders, ", "), cdnBaseURL))
			return
		case "/stats":
			stats := userStats[userID]
			rate := 0.0
			if stats.Total > 0 {
				rate = float64(stats.Success) / float64(stats.Total) * 100
			}
sendMessage(chatID, fmt.Sprintf("📊 上传统计\n\n总计: %d\n成功: %d\n失败: %d\n成功率: %.1f%%", stats.Total, stats.Success, stats.Fail, rate))
	return
	}
}

if msg.Text != "" && !strings.HasPrefix(msg.Text, "/") {
		text := strings.ToLower(strings.TrimSpace(msg.Text))
		if setUserBucket(userID, text) {
			sendMessage(chatID, "✅ 已切换到存储桶: "+text)
			return
		}
		if setUserFolder(userID, text) {
			sendMessage(chatID, "✅ 已切换到文件夹: "+text)
			return
		}
		return
	}

	folder := getUserFolder(userID)
	bucket := getUserBucket(userID)

	if msg.Document != nil {
		doc := msg.Document
		downloadURL, _ := getFile(doc.FileID)
		go processFile(chatID, doc.FileID, doc.FileName, folder, bucket, downloadURL)
		return
	}

	if len(msg.Photo) > 0 {
		photo := msg.Photo[len(msg.Photo)-1]
		downloadURL, _ := getFile(photo.FileID)
		fileName := photo.FileUniqueID + ".jpg"
		go processFile(chatID, photo.FileID, fileName, folder, bucket, downloadURL)
		return
	}

	if msg.Video != nil {
		video := msg.Video
		downloadURL, _ := getFile(video.FileID)
		fileName := video.FileName
		if fileName == "" {
			fileName = "video_" + video.FileUniqueID + ".mp4"
		}
		go processFile(chatID, video.FileID, fileName, folder, bucket, downloadURL)
		return
	}

	if msg.Audio != nil {
		audio := msg.Audio
		downloadURL, _ := getFile(audio.FileID)
		fileName := audio.FileName
		if fileName == "" {
			fileName = "audio_" + audio.FileUniqueID + ".mp3"
		}
		go processFile(chatID, audio.FileID, fileName, folder, bucket, downloadURL)
		return
	}
}

func main() {
	if botToken == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN not set!")
	}

	username, err := getMe()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Bot started: @%s", username)

	if err := setMyCommands(); err != nil {
		log.Printf("Warning: setMyCommands failed: %v", err)
	} else {
		log.Printf("Bot commands registered")
	}

	var offset int64 = 0

	for {
		updates, err := getUpdates(offset)
		if err != nil {
			log.Printf("getUpdates error: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		for _, update := range updates {
			handleUpdate(update)
			offset = update.UpdateID + 1
		}

		if len(updates) == 0 {
			time.Sleep(1 * time.Second)
		}
	}
}