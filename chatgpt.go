package core

import (
	"bufio"
	"bytes"
	"chatgpt/messageQueue"
	"context"
	"encoding/json"
	"errors"
	if_expression "github.com/golang-infrastructure/go-if-expression"
	"github.com/golang-infrastructure/go-pointer"
	"github.com/google/uuid"
	"io"
	"net/http"
	"strings"
	"time"
	"xuelin.me/go/lib/log"
)

// ------------------------------------------------- 请求相关 ------------------------------------------------------------

type Request struct {
	Action          string           `json:"action"`
	Messages        []RequestMessage `json:"messages"`
	ConversationID  *string          `json:"conversation_id"`
	ParentMessageID *string          `json:"parent_message_id"`
	Model           string           `json:"model"`
}

type RequestMessage struct {
	ID      string         `json:"id"`
	Role    string         `json:"role"`
	Content RequestContent `json:"content"`
}

type RequestContent struct {
	ContentType string   `json:"content_type"`
	Parts       []string `json:"parts"`
}

func NewRequest(messageId, question, conversationID, parentMessageID string) *Request {
	return &Request{
		Action:         "next",
		ConversationID: pointer.ToPointerOrNil(conversationID),
		Messages: []RequestMessage{
			{
				ID:   messageId,
				Role: "user",
				Content: RequestContent{
					ContentType: "text",
					Parts:       []string{question},
				},
			},
		},
		ParentMessageID: pointer.ToPointerOrNil(parentMessageID),
		Model:           "text-davinci-002-render",
	}
}

type Response struct {
	Message        ResponseMessage `json:"message"`
	ConversationID string          `json:"conversation_id"`
	Error          any             `json:"error"`
	End            bool            `json:"end"`
}

type ResponseMessage struct {
	ID         string           `json:"id"`
	Role       string           `json:"role"`
	User       any              `json:"user"`
	CreateTime any              `json:"create_time"`
	UpdateTime any              `json:"update_time"`
	Content    ResponseContent  `json:"content"`
	EndTurn    any              `json:"end_turn"`
	Weight     float64          `json:"weight"`
	Metadata   ResponseMetadata `json:"metadata"`
	Recipient  string           `json:"recipient"`
}

type ResponseContent struct {
	ContentType string   `json:"content_type"`
	Parts       []string `json:"parts"`
}

type ResponseMetadata struct {
}

type ChatGPT struct {
	MessageBus         *messageQueue.MemoryMessageQueue
	MessageId          string //本地生成的当前id,为了数据库记录等
	conversationAPIURL string
	jwt                string
	CFSession          string
	authorization      string
	userAgent          string
	conversationID     string
	parentMessageID    string
}

func NewChatGPT(conversationAPIURL, jwt string, messageConsumer *messageQueue.ConsumerConfigure) *ChatGPT {
	gpt := &ChatGPT{
		MessageBus:         messageQueue.NewMemoryMessageQueue("messageNotifyBus", messageQueue.ModeChan),
		conversationAPIURL: conversationAPIURL,
		jwt:                jwt,
		authorization:      "Bearer " + jwt,
		userAgent:          "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/105.0.0.0 Safari/537.36",
	}
	gpt.MessageBus.AddConsumer(messageConsumer)
	return gpt
}

func (chatGPT *ChatGPT) GetMidCidAndPid() (string, string, string) {
	return chatGPT.MessageId, chatGPT.conversationID, chatGPT.parentMessageID
}

func (chatGPT *ChatGPT) Talk(ctx context.Context, question string) (*Response, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			conversationID := if_expression.Return(chatGPT.conversationID != "", chatGPT.conversationID, "")
			parentMessageID := if_expression.Return(chatGPT.parentMessageID != "", chatGPT.parentMessageID, uuid.New().String())
			response, err := chatGPT.GetAnswerWithStream(question, conversationID, parentMessageID)
			if err != nil {
				return nil, err
			}
			chatGPT.conversationID = response.ConversationID
			chatGPT.parentMessageID = response.Message.ID
			return response, nil
		}
	}
}

func (chatGPT *ChatGPT) GetAnswerWithStream(question, conversationID, parentMessageID string) (*Response, error) {
	chatResponse := Response{}
	chatGPT.MessageId = uuid.New().String()
	request := NewRequest(chatGPT.MessageId, question, conversationID, parentMessageID)
	requestBytes, _ := json.Marshal(request)
	req, _ := http.NewRequest("POST", chatGPT.conversationAPIURL, bytes.NewBuffer(requestBytes))
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/100.0.4896.127 Safari/537.36")
	req.Header.Set("Referer", "https://chat.openai.com/chat")
	req.Header.Set("Authorization", chatGPT.authorization)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Openai-Assistant-App-Id", "")
	client := &http.Client{
		Timeout: time.Second * 200,
	}
	resp, err := client.Do(req)
	if err != nil {
		chatResponse.End = true
		chatResponse.Error = "http请求错误" + err.Error()
		chatGPT.MessageBus.Publish(&messageQueue.Message{
			Id:           chatResponse.Message.ID,
			MessageEntry: &chatResponse,
		})
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Info("响应状态错误", resp.Status)
		chatResponse.End = true
		chatResponse.Error = "响应状态错误"
		chatGPT.MessageBus.Publish(&messageQueue.Message{
			Id:           chatResponse.Message.ID,
			MessageEntry: &chatResponse,
		})
		return nil, errors.New(resp.Status)
	}
	// Parse SSE events from response
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				chatResponse.End = true
				if len(chatResponse.Message.Content.Parts) == 0 {
					chatResponse.Error = line
				}
				chatGPT.MessageBus.Publish(&messageQueue.Message{
					Id:           time.Now().UnixNano(),
					MessageEntry: &chatResponse,
				})
				return &chatResponse, nil
			} else {
				return nil, err
			}
		}
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]\n" {
				continue
			}
			err = json.Unmarshal([]byte(data), &chatResponse)
			if err != nil {
				log.Info("unMarshalJSON error ", err, line, data)
				continue
			}
			chatGPT.MessageBus.Publish(&messageQueue.Message{
				Id:           chatResponse.Message.ID,
				MessageEntry: &chatResponse,
			})
		}
	}
	return &chatResponse, nil
}

func (chatGPT *ChatGPT) GetConversationID() string {
	return chatGPT.conversationID
}

func (chatGPT *ChatGPT) SetConversationID(conversationID string) {
	chatGPT.conversationID = conversationID
}

func (chatGPT *ChatGPT) GetParentMessageID() string {
	return chatGPT.parentMessageID
}

func (chatGPT *ChatGPT) SetParentMessageID(parentMessageID string) {
	chatGPT.parentMessageID = parentMessageID
}

func (chatGPT *ChatGPT) GetUserAgent() string {
	return chatGPT.userAgent
}

func (chatGPT *ChatGPT) SetUserAgent(userAgent string) {
	chatGPT.userAgent = userAgent
}

func (chatGPT *ChatGPT) SetJWT(jwt string) {
	chatGPT.jwt = jwt
	chatGPT.authorization = "Bearer " + jwt
}
