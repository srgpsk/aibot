package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"

	"github.com/joho/godotenv"
	gpt "github.com/sashabaranov/go-gpt3"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

var slackClient *slack.Client
var signingSecret string

func main() {
	// Try to load vars from .env is not already present in the system
	if os.Getenv("SLACK_TOKEN") == "" && godotenv.Load() != nil {
		log.Fatal("neither env vars nor .env file exist")
	}
	// You more than likely want your "Bot User OAuth Access Token" which starts with "xoxb-"
	slackClient = slack.New(os.Getenv("SLACK_TOKEN"), slack.OptionDebug(false))
	signingSecret = os.Getenv("SLACK_SIGNING_SECRET")

	http.HandleFunc("/event-listener", handleEventRequest)
	http.HandleFunc("/send-to-channels", handleSendToChannelsRequest)

	log.Println("server listening")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("listen and serve: %v", err)
	}
}

func handleSendToChannelsRequest(w http.ResponseWriter, r *http.Request) {
	// naive quick check
	if r.Method != http.MethodPost || (!strings.HasPrefix(r.Host, "localhost") && !strings.HasPrefix(r.Host, "127.0.0.1")) {
		err := fmt.Errorf("only POST requests from localhost allowed, you requested %s, host: %s", r.Method, r.Host)
		logError(err)
		return
	}

	msg := r.FormValue("message")
	slackChannel := r.FormValue("channel")
	if slackChannel != "" {
		if err := postSlackMessage(slackChannel, slack.MsgOptionText(msg, false)); err != nil {
			logError(err)
		}
		return
	}

	authTestResponse, err := slackClient.AuthTest()
	if err != nil {
		logError(err)
		return
	}
	params := slack.GetConversationsForUserParameters{
		UserID:          authTestResponse.UserID,
		ExcludeArchived: true,
		Limit:           100, // default. Max 100 channels, I don't want to handle cursor right now
	}
	channels, _, err := slackClient.GetConversationsForUser(&params)
	if err != nil {
		logError(err)
		return
	}

	for _, ch := range channels {
		if err := postSlackMessage(ch.GroupConversation.Conversation.ID, slack.MsgOptionText(msg, false)); err != nil {
			logError(err)
		}
	}
}

func handleEventRequest(w http.ResponseWriter, r *http.Request) {
	log.Println("event handler executed")

	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if err := verifySlackSignature(r.Header, body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	eventsAPIEvent, err := slackevents.ParseEvent(json.RawMessage(body), slackevents.OptionNoVerifyToken())
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	log.Printf("event occurred %v", eventsAPIEvent.Type)

	switch eventsAPIEvent.Type {
	case slackevents.URLVerification:
		var res *slackevents.ChallengeResponse
		err := json.Unmarshal([]byte(body), &res)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			logError(err)
			return
		}

		w.Header().Set("Content-Type", "text")
		w.Write([]byte(res.Challenge))
		log.Printf("url verified: %s\n", r.URL)

	case slackevents.CallbackEvent:
		innerEvent := eventsAPIEvent.InnerEvent
		switch ev := innerEvent.Data.(type) {
		case *slackevents.AppMentionEvent:
			log.Printf("mention event occurred %v", ev)

			go handleMentionEvent(ev)
		default:
			// w.WriteHeader(http.StatusInternalServerError)
			logError(fmt.Errorf("unsupported callback event type: %v", ev))
		}
	default:
		// w.WriteHeader(http.StatusInternalServerError)
		logError(fmt.Errorf("unsupported event type: %v", eventsAPIEvent.Type))
		return
	}
}

func verifySlackSignature(header http.Header, body []byte) error {
	sv, err := slack.NewSecretsVerifier(header, signingSecret)
	if err != nil {
		return err
	}
	if _, err := sv.Write(body); err != nil {
		return err
	}
	if err := sv.Ensure(); err != nil {
		return err
	}

	return nil
}

func handleMentionEvent(ev *slackevents.AppMentionEvent) {
	log.Printf("%s user sent text %s", ev.User, ev.Text)
	defer runtime.Goexit()

	var msg slack.MsgOption
	msg = buildSlackAckMessageOption(ev.User, ev.Text)
	if err := postSlackMessage(ev.Channel, msg); err != nil {
		logError(err)
		return
	}
	log.Printf("sent back initial text response %v\n\n", msg)

	if noImageNeeded(ev.Text) {
		return
	}

	prompt := sanitizeImageGenerationPrompt(ev.Text)
	url, err := generateImageUrlByUserText(prompt)
	if err != nil {
		msg := buildSlackSimpleTextMessageOption(ev.User, err.Error())
		postSlackMessage(ev.Channel, msg)
		logError(err)
		return
	}

	// url := "https://i1.wp.com/thetempest.co/wp-content/uploads/2017/08/The-wise-words-of-Michael-Scott-Imgur-2.jpg?w=1024&ssl=1"

	msg = buildSlackImageMessageOption(url, ev.User, prompt)
	log.Printf("prepared image message %v\n\n", msg)

	if err := postSlackMessage(ev.Channel, msg); err != nil {
		logError(err)
		return
	}
	log.Printf("sent image message %v\n\n", msg)
}

func sanitizeImageGenerationPrompt(s string) string {
	// s = strings.Replace(strings.ToLower(s), "imagine", "", -1)
	r := regexp.MustCompile("(?i)imagine")
	s = r.ReplaceAllString(s, "")

	// remove any mentions from the text
	r = regexp.MustCompile("<.+?>")
	for _, txt := range r.FindAllString(s, -1) {
		s = strings.Replace(s, txt, "", -1)
	}

	return strings.TrimSpace(s)
}

func buildSlackSimpleTextMessageOption(user string, text string) slack.MsgOption {
	return slack.MsgOptionText(fmt.Sprintf("<@%s> %s", user, text), false)
}

func buildSlackAckMessageOption(user string, text string) slack.MsgOption {
	var responseText string
	responseText = fmt.Sprintf("<@%s> Got it, let me think...", user)
	if noImageNeeded(text) {
		// responseText = fmt.Sprintf("<@%s> Try to ask me something like: _*imagine*_ intelligent arachnoid sees a human on the wall 😱", user)
		responseText = fmt.Sprintf("<@%s> Try to ask me something like: _*imagine*_ pink unicorn laying eggs in desert", user)
	}

	return slack.MsgOptionText(responseText, false)
}

func buildSlackImageMessageOption(imageUrl, user, text string) slack.MsgOption {
	var blocks []slack.Block
	sectionBlock := slack.NewSectionBlock(&slack.TextBlockObject{
		Type: slack.MarkdownType,
		Text: fmt.Sprintf("<@%s> here's what I imagine about '%s'", user, text),
	}, nil, nil)
	imageBlock := slack.NewImageBlock(imageUrl, text, "", nil)

	blocks = append(blocks, sectionBlock, imageBlock)

	return slack.MsgOptionBlocks(blocks...)
}

func postSlackMessage(ch string, message slack.MsgOption) error {
	if _, _, err := slackClient.PostMessage(ch, message); err != nil {
		return err
	}

	return nil
}

func generateImageUrlByUserText(s string) (string, error) {
	client := gpt.NewClient(os.Getenv("OPENAI_TOKEN"))
	ctx := context.Background()
	req := gpt.ImageRequest{
		Prompt: s,
		N:      1,
		Size:   gpt.CreateImageSize256x256,
	}

	res, err := client.CreateImage(ctx, req)
	if err != nil {
		return "", err
	}

	return res.Data[0].URL, nil
}

func noImageNeeded(s string) bool {
	return !strings.Contains(strings.ToLower(s), "imagine")
}

func logError(err error) {
	log.Printf("[ERROR] %v\n\n", err)
}
