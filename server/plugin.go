package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

// Plugin implements the interface expected by the Mattermost server to communicate between the server and plugin processes.
type Plugin struct {
	plugin.MattermostPlugin

	// configurationLock synchronizes access to the configuration.
	configurationLock sync.RWMutex

	// configuration is the active plugin configuration. Consult getConfiguration and
	// setConfiguration for usage.
	configuration *configuration
}

type messageStruct struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type respBody struct {
	Id      string         `json:"id"`
	Object  string         `json:"object"`
	Created int            `json:"created"`
	Model   string         `json:"model"`
	Usage   usageStruct    `json:"usage"`
	Choices []choiceStruct `json:"choices"`
}

type choiceStruct struct {
	Message      messageStruct `json:"message"`
	FinishReason string        `json:"finish_reason"`
	Index        int           `json:"index"`
}

type usageStruct struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type reqBody struct {
	Model     string          `json:"model"`
	Messages  []messageStruct `json:"messages"`
	MaxTokens int             `json:"max_tokens"`
}

type OpenAIRequest struct {
	AssistantID string `json:"assistant_id"`
	Thread      Thread `json:"thread"`
	Stream      bool   `json:"stream"`
}

type Thread struct {
	Messages []Message `json:"messages"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ConversationMemory struct {
	mu            sync.RWMutex
	Conversations map[string][]Message // map ChannelID -> list of messages
}

type ContentText struct {
	Value string `json:"value"`
}

type Content struct {
	Type string      `json:"type"`
	Text ContentText `json:"text"`
}

type Event struct {
	Content []Content `json:"content"`
}

// ServeHTTP demonstrates a plugin that handles HTTP requests by greeting the world.
func (p *Plugin) ServeHTTP(c *plugin.Context, w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "Hello, world!")
}

func NewConversationMemory() *ConversationMemory {
	return &ConversationMemory{
		Conversations: make(map[string][]Message),
	}
}

var conversationMemory = NewConversationMemory()

func (cm *ConversationMemory) GetConversation(channelID string) []Message {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.Conversations[channelID]
}

const MaxMessages = 50

func (cm *ConversationMemory) StoreConversation(channelID string, message Message) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if len(cm.Conversations[channelID]) >= MaxMessages {
		cm.Conversations[channelID] = cm.Conversations[channelID][1:]
	}
	cm.Conversations[channelID] = append(cm.Conversations[channelID], message)
}

// callOpenAI calls the OpenAI API with streaming and handles the response
func (p *Plugin) callOpenAI(ctx context.Context, prompt string) (string, error) {
	configuration := p.getConfiguration()

	reqBody := OpenAIRequest{
		AssistantID: configuration.ASSISTANT_ID,
		Thread: Thread{
			Messages: []Message{
				{
					Role:    "user",
					Content: prompt,
				},
			},
		},
		Stream: true,
	}

	reqData, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", configuration.PROXY_URL+"/v1/threads/runs", bytes.NewBuffer(reqData))
	if err != nil {
		return "", fmt.Errorf("failed to create new request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+configuration.SECRET_KEY)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "assistants=v2")

	client := &http.Client{
		Timeout: 60 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return p.processResponse(resp.Body)
}

func RemoveControlCharacters(content string) string {
	arrControlCharacters := [4]string{`\\b`, `\b`, `\\v`, `\v`}
	for _, character := range arrControlCharacters {
		content = strings.ReplaceAll(content, character, "")
	}
	return content
}

func HandleControlCharacters(content string) string {
	arrRegexp := [3]string{`[\f]`, `[\t]`, `[\r\n]`}
	arrReplace := [3]string{"\\f", "\\t", "\\n"}

	for i, character := range arrRegexp {
		matchNewlines := regexp.MustCompile(character)
		escapeNewlines := func(s string) string {
			return matchNewlines.ReplaceAllString(s, arrReplace[i])
		}
		re := regexp.MustCompile(`"[^"\\]*(?:\\[\s\S][^"\\]*)*"`)
		content = re.ReplaceAllStringFunc(content, escapeNewlines)
	}
	return RemoveControlCharacters(content)
}

// processResponse processes the response from the OpenAI stream
func (p *Plugin) processResponse(body io.Reader) (string, error) {
	reader := bufio.NewReader(body)
	var result string

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", err
		}

		if strings.HasPrefix(line, "data: [DONE]") {
			log.Println("stop")
			break
		}

		if strings.HasPrefix(line, "event: thread.message.completed") {
			dataLine, err := reader.ReadString('\n')
			if err != nil {
				return "", err
			}

			if strings.HasPrefix(dataLine, "data: ") {
				data := strings.TrimPrefix(dataLine, "data: ")

				var event Event
				if err := json.Unmarshal([]byte(data), &event); err != nil {
					log.Println("Error unmarshalling data:", err)
					continue
				}

				for _, content := range event.Content {
					if content.Type == "text" {
						result = content.Text.Value
					}
				}
			}
		}
	}

	return result, nil
}

func ConvertEmojToHTML(input string) string {
	res := ""
	runes := []rune(input)

	for _, r := range runes {
		if r < 128 {
			res += string(r)
		} else {
			res += "&#" + strconv.FormatInt(int64(r), 10) + ";"
		}
	}
	return res
}

func ConvertTextAndEmojToHTML(input string) string {
	emojiRegex := regexp.MustCompile(`[\x{1F600}-\x{1F64F}]|[\x{1F300}-\x{1F5FF}]|[\x{1F680}-\x{1F6FF}]|[\x{1F700}-\x{1F77F}]|[\x{1F780}-\x{1F7FF}]|[\x{1F800}-\x{1F8FF}]|[\x{1F900}-\x{1F9FF}]|[\x{1FA00}-\x{1FA6F}]`)

	result := ""
	lastIndex := 0

	for _, match := range emojiRegex.FindAllStringIndex(input, -1) {
		start, end := match[0], match[1]
		result += input[lastIndex:start]
		result += ConvertEmojToHTML(input[start:end]) // move emoji to HTML entity
		lastIndex = end
	}

	result += input[lastIndex:]

	return result
}

func (p *Plugin) MessageHasBeenPosted(c *plugin.Context, post *model.Post) {
	configuration := p.getConfiguration()
	re := regexp.MustCompile(`(^|\s)@vareal\.angel(\s|$)`)
	if re.MatchString(post.Message) {
		if post.Type == "custom_chatgpt_plugin" {
			return
		}

		rootId := post.RootId
		if post.RootId == "" {
			rootId = post.Id
		}

		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()

		// get previous conversation
		previousMessages := conversationMemory.GetConversation(post.ChannelId)

		prompt := ""
		for _, msg := range previousMessages {
			prompt += msg.Role + ": " + msg.Content + "\n"
		}
		prompt += "user: " + post.Message

		responseMessage, err := p.callOpenAI(ctx, prompt)

		if err != nil {
			log.Println("Error calling OpenAI API:", err)
			return
		}

		conversationMemory.StoreConversation(post.ChannelId, Message{Role: "user", Content: post.Message})
		conversationMemory.StoreConversation(post.ChannelId, Message{Role: "assistant", Content: responseMessage})

		log.Println("day la ket qua truoc khi format", responseMessage)

		// cleanedMessage := strings.ToValidUTF8(responseMessage, "")

		cleanedMessage := ConvertTextAndEmojToHTML(responseMessage)

		log.Println("day la ket qua sau khi format", cleanedMessage)

		// cleanedMessage = regexp.MustCompile(`[^\p{L}\p{N}\s.,!*?~#->\x60]`).ReplaceAllString(cleanedMessage, "")

		post := &model.Post{
			ChannelId: post.ChannelId,
			RootId:    rootId,
			UserId:    configuration.BOT_ID,
			Message:   cleanedMessage,
			Type:      "custom_chatgpt_plugin",
		}
		createdPost, appErr := p.API.CreatePost(post)

		if appErr != nil {
			log.Println("Error creating post:", appErr.Error())

			errorPost := &model.Post{
				ChannelId: post.ChannelId,
				RootId:    rootId,
				UserId:    configuration.BOT_ID,
				Message:   fmt.Sprintf("Failed to create post: %s", appErr.Error()),
				Type:      "custom_chatgpt_plugin",
			}

			_, logErr := p.API.CreatePost(errorPost)
			if logErr != nil {
				log.Println("Error creating error log post:", logErr.Error())
			} else {
				log.Println("Created error log post successfully")
			}
		} else {
			log.Println("Created post successfully:", createdPost)
		}
	}
}

// See https://developers.mattermost.com/extend/plugins/server/reference/
