package googlechat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"cloud.google.com/go/pubsub"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/identity"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/utils"
	chat "google.golang.org/api/chat/v1"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
	"sync"
)

func init() {
	channels.RegisterFactory("googlechat", func(cfg *config.Config, bus *bus.MessageBus) (channels.Channel, error) {
		return NewGoogleChatChannel(cfg.Channels.GoogleChat, bus)
	})
}

type GoogleChatChannel struct {
	*channels.BaseChannel
	config        config.GoogleChatConfig
	pubsubClient  *pubsub.Client
	chatService   *chat.Service
	ctx           context.Context
	cancel        context.CancelFunc
	activeThreads map[string]string // threadName -> messageName (for updating status)
	mu            sync.RWMutex      // protects activeThreads
}

// PubSubMessagePayload represents the structure of the message data received from Pub/Sub
// The actual payload is inside the "data" field of the PubSub message, which is a JSON string of the event.
// However, the Google Chat event is sent as the message body directly if configured as "Cloud Pub/Sub" endpoint.
// Let's assume the standard Google Chat Event format.

// GoogleChatUser defines the user structure with Email field which might be missing in chat.User
type GoogleChatUser struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	Type        string `json:"type"`
	DomainID    string `json:"domainId"`
}

// GoogleChatMessage defines the message structure to capture Sender with Email
type GoogleChatMessage struct {
	Name         string             `json:"name"`
	Sender       *GoogleChatUser    `json:"sender"`
	Text         string             `json:"text"`
	ArgumentText string             `json:"argumentText"`
	Thread       *chat.Thread       `json:"thread"`
	Attachments  []*chat.Attachment `json:"attachments,omitempty"`
}

func (m *GoogleChatMessage) UnmarshalJSON(data []byte) error {
	type Alias GoogleChatMessage
	aux := &struct {
		Attachment []*chat.Attachment `json:"attachment"`
		*Alias
	}{
		Alias: (*Alias)(m),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	// Fallback to "attachment" if "attachments" is empty
	if len(aux.Attachment) > 0 && len(m.Attachments) == 0 {
		m.Attachments = aux.Attachment
	}
	return nil
}

// GoogleChatEvent supports both Classic and Google Workspace event formats
type GoogleChatEvent struct {
	// Classic format fields
	Name    string             `json:"name"`
	Type    string             `json:"type"`
	Space   *chat.Space        `json:"space"`
	Message *GoogleChatMessage `json:"message"`
	User    *GoogleChatUser    `json:"user"`

	// Google Workspace Event format fields
	Chat *struct {
		MessagePayload *struct {
			Message *GoogleChatMessage `json:"message"`
			Space   *chat.Space        `json:"space"`
		} `json:"messagePayload"`
		AddedToSpacePayload *struct {
			Space *chat.Space `json:"space"`
		} `json:"addedToSpacePayload"`
		User *GoogleChatUser `json:"user"`
	} `json:"chat"`
}

func NewGoogleChatChannel(cfg config.GoogleChatConfig, messageBus *bus.MessageBus) (*GoogleChatChannel, error) {
	if cfg.SubscriptionID == "" {
		return nil, fmt.Errorf("google chat subscription_id is required")
	}

	base := channels.NewBaseChannel("googlechat", cfg, messageBus, cfg.AllowFrom,
		channels.WithReasoningChannelID(cfg.ReasoningChannelID),
	)

	gcc := &GoogleChatChannel{
		BaseChannel:   base,
		config:        cfg,
		activeThreads: make(map[string]string),
	}
	base.SetOwner(gcc)

	return gcc, nil
}

func (c *GoogleChatChannel) Start(ctx context.Context) error {
	logger.InfoC("googlechat", "Starting Google Chat channel")

	c.ctx, c.cancel = context.WithCancel(ctx)

	// Initialize Chat Service
	// We use ADC (Application Default Credentials)
	var opts []option.ClientOption
	opts = append(opts, option.WithScopes("https://www.googleapis.com/auth/chat.bot"))

	// NOTE: If PubSub receive fails with Unauthenticated during local impersonated testing, run:
	// gcloud auth application-default login
	if impersonateAccount := os.Getenv("GOOGLE_IMPERSONATE_SERVICE_ACCOUNT"); impersonateAccount != "" {
		logger.InfoCF("googlechat", "Using Impersonated Credentials", map[string]any{
			"account": impersonateAccount,
		})
		ts, err := impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
			TargetPrincipal: impersonateAccount,
			Scopes:          []string{"https://www.googleapis.com/auth/chat.bot"},
		})
		if err != nil {
			c.cancel()
			return fmt.Errorf("failed to create impersonated token source: %w", err)
		}
		opts = append(opts, option.WithTokenSource(ts))
	}

	chatService, err := chat.NewService(ctx, opts...)
	if err != nil {
		c.cancel()
		return fmt.Errorf("failed to create google chat service: %w", err)
	}
	c.chatService = chatService

	// Initialize Pub/Sub Client
	projectID := c.config.ProjectID
	if projectID == "" {
		// If project ID is not specified, we can try to detect it, OR just require it.
		// Pubsub client requires a project ID.
		// Let's try to parse it from the subscription ID if it's a full path
		// projects/{project}/subscriptions/{sub}
		if strings.HasPrefix(c.config.SubscriptionID, "projects/") {
			parts := strings.Split(c.config.SubscriptionID, "/")
			if len(parts) >= 4 && parts[0] == "projects" && parts[2] == "subscriptions" {
				projectID = parts[1]
			}
		}
	}

	if projectID == "" {
		// Fallback to detection
		projectID = pubsub.DetectProjectID
	}

	pubsubClient, err := pubsub.NewClient(ctx, projectID)
	if err != nil {
		c.cancel()
		return fmt.Errorf("failed to create pubsub client: %w", err)
	}
	c.pubsubClient = pubsubClient

	// Start receiving messages
	go c.receiveLoop()

	c.SetRunning(true)
	logger.InfoC("googlechat", "Google Chat channel started")
	return nil
}

func (c *GoogleChatChannel) Stop(ctx context.Context) error {
	logger.InfoC("googlechat", "Stopping Google Chat channel")

	if c.cancel != nil {
		c.cancel()
	}

	if c.pubsubClient != nil {
		c.pubsubClient.Close()
	}

	c.SetRunning(false)
	logger.InfoC("googlechat", "Google Chat channel stopped")
	return nil
}
func (c *GoogleChatChannel) MaxMessageLength() int {
	return 4000
}

func (c *GoogleChatChannel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if !c.IsRunning() {
		return fmt.Errorf("googlechat channel not running")
	}

	// msg.ChatID is expected to be the Thread Name or Space Name
	// Format: "spaces/SPACE_ID/threads/THREAD_ID" or "spaces/SPACE_ID"

	var spaceName string
	var threadName string

	if strings.Contains(msg.ChatID, "/threads/") {
		parts := strings.Split(msg.ChatID, "/threads/")
		if len(parts) == 2 {
			spaceName = parts[0]
			threadName = msg.ChatID
		}
	} else {
		spaceName = msg.ChatID
	}

	// Status Update Handling
	if msg.Type == "status" {
		formattedText := toChatFormat(msg.Content)
		if formattedText == "" {
			return nil
		}

		// Check if we already have an active status message for this chat
		c.mu.Lock()
		activeMsgName, exists := c.activeThreads[msg.ChatID]
		c.mu.Unlock()

		if exists {
			logger.DebugCF("googlechat", "Updating status message", map[string]any{
				"chat_id": msg.ChatID,
				"name":    activeMsgName,
			})
			// Update existing message
			chatMsg := &chat.Message{
				Text: formattedText,
			}
			_, err := c.chatService.Spaces.Messages.Update(activeMsgName, chatMsg).UpdateMask("text").Context(ctx).Do()
			if err != nil {
				logger.ErrorCF("googlechat", "Failed to update status message", map[string]any{
					"error":   err.Error(),
					"chat_id": msg.ChatID,
					"name":    activeMsgName,
				})
				c.mu.Lock()
				delete(c.activeThreads, msg.ChatID)
				c.mu.Unlock()
			}
			return nil
		} else {
			// Create new status message
			logger.DebugCF("googlechat", "Creating status message", map[string]any{
				"chat_id": msg.ChatID,
			})
			chatMsg := &chat.Message{
				Text: formattedText,
			}
			if threadName != "" {
				chatMsg.Thread = &chat.Thread{
					Name: threadName,
				}
			}
			resp, err := c.chatService.Spaces.Messages.Create(spaceName, chatMsg).Context(ctx).Do()
			if err != nil {
				return fmt.Errorf("failed to create status message: %w", err)
			}
			c.mu.Lock()
			c.activeThreads[msg.ChatID] = resp.Name
			c.mu.Unlock()
			logger.DebugCF("googlechat", "Status message created", map[string]any{
				"chat_id": msg.ChatID,
				"name":    resp.Name,
			})
			return nil
		}
	}

	// Final Message Handling (Type == "message" or empty)
	chatMsg := &chat.Message{
		Text: toChatFormat(msg.Content),
	}

	if chatMsg.Text == "" {
		return nil
	}

	if threadName != "" || msg.ChatID != "" {
		// If we have an active status message, update it with the final response
		c.mu.Lock()
		activeMsgName, exists := c.activeThreads[msg.ChatID]
		if exists {
			delete(c.activeThreads, msg.ChatID)
		}
		c.mu.Unlock()

		if exists {
			logger.DebugCF("googlechat", "Converting status message to final response", map[string]any{
				"chat_id": msg.ChatID,
				"name":    activeMsgName,
			})
			chatMsg.Thread = nil // Update targets message name
			_, err := c.chatService.Spaces.Messages.Update(activeMsgName, chatMsg).UpdateMask("text").Context(ctx).Do()
			if err == nil {
				return nil
			}
			// If update failed, fall through to create new message
			logger.ErrorCF("googlechat", "Failed to update status to final message", map[string]any{
				"error": err.Error(),
				"name":  activeMsgName,
			})
		}

		if threadName != "" {
			chatMsg.Thread = &chat.Thread{
				Name: threadName,
			}
		}
	}

	_, err := c.chatService.Spaces.Messages.Create(spaceName, chatMsg).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("failed to send google chat message: %w", err)
	}

	logger.DebugCF("googlechat", "Message sent", map[string]any{
		"space":  spaceName,
		"thread": threadName,
	})

	return nil
}

func (c *GoogleChatChannel) receiveLoop() {
	// Handle full subscription path vs just ID
	subID := c.config.SubscriptionID
	if strings.Contains(subID, "/") {
		if strings.HasPrefix(subID, "projects/") {
			parts := strings.Split(subID, "/")
			if len(parts) >= 4 {
				subID = parts[3]
			}
		}
	}

	logger.InfoCF("googlechat", "Starting Pub/Sub receive loop", map[string]any{
		"subscription_id": subID,
	})

	sub := c.pubsubClient.Subscription(subID)
	sub.ReceiveSettings.MaxOutstandingMessages = 10

	err := sub.Receive(c.ctx, func(ctx context.Context, msg *pubsub.Message) {
		msg.Ack() // Ack immediately. If we crash, we lose the message, but prevents loops.

		logger.DebugCF("googlechat", "Raw PubSub payload received", map[string]any{
			"data":       string(msg.Data),
			"attributes": msg.Attributes,
		})

		var event GoogleChatEvent
		if err := json.Unmarshal(msg.Data, &event); err != nil {
			logger.ErrorCF("googlechat", "Failed to unmarshal pubsub message event", map[string]any{
				"error": err.Error(),
			})
			return
		}

		c.handleEvent(event)
	})

	if err != nil && c.ctx.Err() == nil {
		logger.ErrorCF("googlechat", "PubSub receive error", map[string]any{
			"error": err.Error(),
		})
	}
}

func (c *GoogleChatChannel) handleEvent(event GoogleChatEvent) {
	// Normalize Google Workspace Event to Classic format if needed
	if event.Type == "" && event.Chat != nil {
		if payload := event.Chat.MessagePayload; payload != nil {
			event.Type = "MESSAGE"
			event.Message = payload.Message
			event.Space = payload.Space
			event.User = event.Chat.User
			logger.InfoC("googlechat", "Normalized Workspace MESSAGE event")
		} else if payload := event.Chat.AddedToSpacePayload; payload != nil {
			event.Type = "ADDED_TO_SPACE"
			event.Space = payload.Space
			event.User = event.Chat.User
			logger.InfoC("googlechat", "Normalized Workspace ADDED_TO_SPACE event")
		}
	}

	logger.DebugCF("googlechat", "Received event", map[string]any{
		"type": event.Type,
		"name": event.Name,
	})

	switch event.Type {
	case "MESSAGE":
		c.handleMessage(event)
	case "ADDED_TO_SPACE":
		c.handleAddedToSpace(event)
	}
}

func (c *GoogleChatChannel) handleMessage(event GoogleChatEvent) {
	if event.Message == nil {
		return
	}

	// Use event-level user if message-level sender is missing
	if event.Message.Sender == nil {
		if event.User != nil {
			event.Message.Sender = event.User
		} else {
			return
		}
	}

	if event.Message.Sender.Type == "BOT" {
		logger.DebugCF("googlechat", "Skipping message from bot", map[string]any{
			"sender": event.Message.Sender.DisplayName,
		})
		return
	}

	senderName := event.Message.Sender.Name
	senderEmail := event.Message.Sender.Email
	messageID := event.Message.Name

	// Create structured SenderInfo for unified allowcheck and routing
	senderInfo := bus.SenderInfo{
		Platform:    "googlechat",
		PlatformID:  senderName,
		CanonicalID: identity.BuildCanonicalID("googlechat", senderName),
		DisplayName: event.Message.Sender.DisplayName,
	}

	// Access Control (Check SenderInfo first)
	if !c.IsAllowedSender(senderInfo) && (senderEmail == "" || !c.IsAllowed(senderEmail)) {
		logger.WarnCF("googlechat", "Message rejected by allowlist", map[string]any{
			"sender_name": senderName,
			"email":       senderEmail,
		})
		return
	}

	spaceName := ""
	if event.Space != nil {
		spaceName = event.Space.Name
	}

	threadName := ""
	if event.Message.Thread != nil {
		threadName = event.Message.Thread.Name
	}

	// Canonical ChatID for reply: The Thread Name (which includes space)
	chatID := threadName
	if chatID == "" {
		chatID = spaceName
	}

	content := event.Message.ArgumentText
	if content == "" {
		content = event.Message.Text
	}
	content = strings.TrimSpace(content)

	if content == "" && len(event.Message.Attachments) == 0 {
		return
	}

	var mediaFiles []string
	if len(event.Message.Attachments) > 0 {
		if c.config.DownloadAttachments {
			scope := channels.BuildMediaScope("googlechat", chatID, messageID)
			for _, att := range event.Message.Attachments {
				if path, err := c.downloadAttachment(att); err == nil {
					if ms := c.GetMediaStore(); ms != nil {
						ref, err := ms.Store(path, media.MediaMeta{
							Filename:    att.ContentName,
							ContentType: "", // Let it infer or we don't know for sure yet
							Source:      "googlechat",
						}, scope)
						if err == nil {
							mediaFiles = append(mediaFiles, ref)
						} else {
							logger.ErrorCF("googlechat", "Failed to store media in MediaStore", map[string]any{
								"error": err.Error(),
								"path":  path,
							})
						}
					} else {
						// Fallback to raw path if no MediaStore (unlikely)
						mediaFiles = append(mediaFiles, path)
					}
				} else {
					logger.ErrorCF("googlechat", "Failed to download attachment", map[string]any{
						"error": err.Error(),
						"name":  att.ContentName,
					})
				}
			}
		} else {
			logger.WarnCF("googlechat", "Attachments received but download_attachments is disabled in config", map[string]any{
				"count": len(event.Message.Attachments),
			})
		}
	}

	if content == "" && len(mediaFiles) == 0 {
		if len(event.Message.Attachments) > 0 {
			content = "[image]"
		} else {
			logger.DebugCF("googlechat", "Skipping message with no content and no attachments", nil)
			return
		}
	}

	// Metadata
	metadata := map[string]string{
		"platform":     "googlechat",
		"space_name":   spaceName,
		"thread_name":  threadName,
		"sender_name":  event.Message.Sender.DisplayName,
		"sender_email": senderEmail,
		"user_type":    event.Message.Sender.Type,
	}

	var peer bus.Peer
	if event.Space != nil {
		if event.Space.Type == "DM" {
			peer = bus.Peer{Kind: "direct", ID: senderName}
		} else {
			peer = bus.Peer{Kind: "group", ID: spaceName}
		}
	}

	logger.InfoCF("googlechat", "Processing message", map[string]any{
		"sender":  senderEmail,
		"chat_id": chatID,
		"text":    utils.Truncate(content, 50),
		"media":   mediaFiles,
	})

	c.HandleMessage(c.ctx, peer, messageID, senderName, chatID, content, mediaFiles, metadata, senderInfo)
}

func (c *GoogleChatChannel) downloadAttachment(att *chat.Attachment) (string, error) {
	if att.AttachmentDataRef == nil || att.AttachmentDataRef.ResourceName == "" {
		return "", fmt.Errorf("no attachment data reference")
	}

	resourceName := att.AttachmentDataRef.ResourceName
	logger.DebugCF("googlechat", "Downloading attachment", map[string]any{
		"resource_name": resourceName,
		"content_name":  att.ContentName,
	})

	mediaDir := media.TempDir()
	if err := os.MkdirAll(mediaDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create media dir %q: %w", mediaDir, err)
	}

	tmpFile, err := os.CreateTemp(mediaDir, "gchat-att-*.bin")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file in %q: %w", mediaDir, err)
	}
	defer tmpFile.Close()

	// Using the Media API to download
	// In the Go client, we use .Download() to get the *http.Response
	resp, err := c.chatService.Media.Download(resourceName).Context(c.ctx).Download()
	if err != nil {
		return "", fmt.Errorf("failed to download media: %w", err)
	}
	defer resp.Body.Close()

	_, err = io.Copy(tmpFile, resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to write to temp file: %w", err)
	}

	finalPath := tmpFile.Name()
	if att.ContentName != "" {
		sanitizedName := filepath.Base(att.ContentName)
		newPath := filepath.Join(filepath.Dir(finalPath), fmt.Sprintf("gchat-%s", sanitizedName))
		if err := os.Rename(finalPath, newPath); err == nil {
			finalPath = newPath
		}
	}

	return finalPath, nil
}

func (c *GoogleChatChannel) handleAddedToSpace(event GoogleChatEvent) {
	if event.Space == nil || event.User == nil {
		return
	}
	logger.InfoCF("googlechat", "Bot added to space", map[string]any{
		"space": event.Space.Name,
		"user":  event.User.DisplayName,
	})
}

var (
	chatBoldRegex = regexp.MustCompile(`\*\*(.*?)\*\*`)
	chatLinkRegex = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
)

func toChatFormat(text string) string {
	if text == "" {
		return ""
	}
	// Bold: **text** -> *text*
	text = chatBoldRegex.ReplaceAllString(text, "*$1*")

	// Links: [text](url) -> <url|text>
	text = chatLinkRegex.ReplaceAllString(text, "<$2|$1>")

	return text
}
