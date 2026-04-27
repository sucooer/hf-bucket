package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	botToken   = os.Getenv("TELEGRAM_BOT_TOKEN")
	hfToken    = os.Getenv("HF_TOKEN")
	hfRepoID   = os.Getenv("HF_REPO_ID")
	cdnBaseURL = os.Getenv("CDN_BASE_URL")
	hfFolders  = os.Getenv("HF_FOLDERS")
)

var defaultFolders = []string{"images", "videos", "documents", "others"}
var userFolders = make(map[int64]string)
var userStats = make(map[int64]*UserStats)

type UserStats struct {
	Total   int `json:"total"`
	Success int `json:"success"`
	Fail    int `json:"fail"`
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

func uploadToHF(filePath, fileName, folder string) (string, error) {
	pathInRepo := folder + "/" + fileName
	uploadURL := fmt.Sprintf("https://huggingface.co/datasets/%s/upload/main/%s", hfRepoID, pathInRepo)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	part, err := writer.CreateFormFile("file", filepath.Base(fileName))
	if err != nil {
		return "", err
	}

	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	io.Copy(part, f)
	writer.Close()

	req, err := http.NewRequest("POST", uploadURL, body)
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", "Bearer "+hfToken)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HF API error: %d - %s", resp.StatusCode, string(bodyBytes))
	}

	if cdnBaseURL != "" {
		return fmt.Sprintf("%s/%s", cdnBaseURL, pathInRepo), nil
	}
	return fmt.Sprintf("https://huggingface.co/datasets/%s/resolve/main/%s", hfRepoID, pathInRepo), nil
}

func processFile(chatID int64, fileID, fileName, folder, downloadURL string) {
	if _, ok := userStats[chatID]; !ok {
		userStats[chatID] = &UserStats{}
	}
	userStats[chatID].Total++

	sendMessage(chatID, "⏳ 正在处理: "+fileName+"...")
	time.Sleep(500 * time.Millisecond)

	tmpDir := "/tmp/hf_uploads"
	os.MkdirAll(tmpDir, 0755)
	localPath := filepath.Join(tmpDir, fileName)

	resp, err := http.Get(downloadURL)
	if err != nil {
		editMessageText(chatID, 0, "❌ 下载失败: "+err.Error())
		userStats[chatID].Fail++
		return
	}
	defer resp.Body.Close()

	out, err := os.Create(localPath)
	if err != nil {
		editMessageText(chatID, 0, "❌ 创建文件失败: "+err.Error())
		userStats[chatID].Fail++
		return
	}
	defer out.Close()

	io.Copy(out, resp.Body)

	editMessageText(chatID, 0, "⏳ 上传到 "+folder+"/"+fileName+"...")
	time.Sleep(500 * time.Millisecond)

	cdnURL, err := uploadToHF(localPath, fileName, folder)
	if err != nil {
		editMessageText(chatID, 0, "❌ 上传失败: "+err.Error())
		userStats[chatID].Fail++
		os.Remove(localPath)
		return
	}

	os.Remove(localPath)
	userStats[chatID].Success++

	editMessageText(chatID, 0, fmt.Sprintf("✅ 上传成功！\n\n📁 文件夹: %s\n🔗 %s", folder, cdnURL))
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
			sendMessage(chatID, "👋 欢迎！我是 Hugging Face 上传 Bot\n\n发送文件直接上传\n/folder - 切换文件夹\n/folders - 查看所有文件夹")
			return
		case "/help":
			sendMessage(chatID, "📖 使用帮助\n\n发送文件（图片、视频、音频、文档）我会自动上传到 Hugging Face\n\n📁 文件夹：images, videos, documents, others")
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
		case "/status":
			folders := getFolders()
			sendMessage(chatID, fmt.Sprintf("⚙️ 当前状态\n\n📁 文件夹: %s\n📂 可用: %s\n🔗 CDN: %s\n📦 仓库: %s",
				getUserFolder(userID), strings.Join(folders, ", "), cdnBaseURL, hfRepoID))
			return
		case "/stats":
			stats := userStats[userID]
			rate := 0.0
			if stats.Total > 0 {
				rate = float64(stats.Success) / float64(stats.Total) * 100
			}
			sendMessage(chatID, fmt.Sprintf("📊 上传统计\n\n总计: %d\n成功: %d\n失败: %d\n成功率: %.1f%%",
				stats.Total, stats.Success, stats.Fail, rate))
			return
		}
	}

	if msg.Text != "" && !strings.HasPrefix(msg.Text, "/") {
		text := strings.ToLower(strings.TrimSpace(msg.Text))
		if setUserFolder(userID, text) {
			sendMessage(chatID, "✅ 已切换到文件夹: "+text)
		}
		return
	}

	folder := getUserFolder(userID)

	if msg.Document != nil {
		doc := msg.Document
		downloadURL, _ := getFile(doc.FileID)
		go processFile(chatID, doc.FileID, doc.FileName, folder, downloadURL)
		return
	}

	if len(msg.Photo) > 0 {
		photo := msg.Photo[len(msg.Photo)-1]
		downloadURL, _ := getFile(photo.FileID)
		fileName := photo.FileUniqueID + ".jpg"
		go processFile(chatID, photo.FileID, fileName, folder, downloadURL)
		return
	}

	if msg.Video != nil {
		video := msg.Video
		downloadURL, _ := getFile(video.FileID)
		fileName := video.FileName
		if fileName == "" {
			fileName = "video_" + video.FileUniqueID + ".mp4"
		}
		go processFile(chatID, video.FileID, fileName, folder, downloadURL)
		return
	}

	if msg.Audio != nil {
		audio := msg.Audio
		downloadURL, _ := getFile(audio.FileID)
		fileName := audio.FileName
		if fileName == "" {
			fileName = "audio_" + audio.FileUniqueID + ".mp3"
		}
		go processFile(chatID, audio.FileID, fileName, folder, downloadURL)
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