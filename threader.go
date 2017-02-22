// Package threader provides a Slick plugin that keeps track of threads in
// Slack and presents a way of quering those threads.  Returned threads have
// the original message and a link to open the thread.
//
// Right now, the only query that works is the word "threads".  When said to
// the bot in a private message, the bot will reply with a list of threads that
// had new messages since the last time the user asked.
package threader

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/abourget/slick"
	"github.com/nlopes/slack"
)

const (
	cutoffDays   = 3
	saveInterval = 30 * time.Minute
)

// threader is the main plugin struct.  It holds the bot reference as well as
// the channels.
type threader struct {
	bot             *slick.Bot
	messagesIn      chan *slick.Message
	summaryRequests chan *summaryRequest
}

// summaryRequest is a struct representing a thread summary request from a
// user.
type summaryRequest struct {
	user   string
	ts     string
	text   string
	result chan summary
}

// summary is a struct representing the thread summaries and count in answer to
// a summary request.
type summary struct {
	count   int
	threads []string
}

func init() {
	slick.RegisterPlugin(&threader{})
}

// InitPlugin is called by the Slick bot framework to initialize the plugin.
// This sets up two handlers, one for recording information about threaded
// messages and the other for accepting queries about the threaded messages.
func (t *threader) InitPlugin(bot *slick.Bot) {
	t.bot = bot

	t.messagesIn = make(chan *slick.Message, 10)
	t.summaryRequests = make(chan *summaryRequest, 10)

	go t.tabulator()

	var err error

	err = bot.Listen(&slick.Listener{
		MentionsMeOnly:     true,
		MessageHandlerFunc: t.queryHandler,
	})
	if err != nil {
		return
	}

	err = bot.Listen(&slick.Listener{
		MessageHandlerFunc: t.threadListener,
	})
	if err != nil {
		return
	}
}

// queryHandler handles any queries.  It does this by sending a summaryRequest
// to the tabulator via a channel.  Then it waits for a response and formats it
// for the user.
func (t *threader) queryHandler(listen *slick.Listener, msg *slick.Message) {
	if msg.Contains("threads") {

		summaryRequest := &summaryRequest{
			user:   msg.User,
			ts:     msg.Timestamp,
			text:   msg.Text,
			result: make(chan summary),
		}

		t.summaryRequests <- summaryRequest
		result := <-summaryRequest.result

		if result.count == 0 {
			msg.ReplyPrivately("No new threads since last time.")
		} else {
			// fmt.Println(result)
			imChannel := t.bot.OpenIMChannelWith(t.bot.GetUser(msg.User))
			if imChannel == nil {
				log.Println("unable to get channel")
			}

			postParams := slack.NewPostMessageParameters()
			postParams.UnfurlMedia = false
			postParams.Username = "botbot"
			var attachments []slack.Attachment
			for _, summary := range result.threads {
				attachments = append(attachments, slack.Attachment{Text: summary})
			}
			postParams.Attachments = attachments

			_, _, err := t.bot.Slack.PostMessage(imChannel.ID, fmt.Sprintf("Hey, there've been %d threads since you asked me last time:", result.count), postParams)
			if err != nil {
				log.Printf("Unable to reply to %s", msg.User)
			}
		}
	}

	return
}

// threadListener watches for message that are part of a thread, as indicated
// by a non-zero length ThreadTimestamp field.  It also listens for a second
// type of message that Slack sends that indicates that a particular thread has
// been responded to.  This second type of message is indicated by the presence
// of a SubMessage with a non-zero length ThreadTimestamp field.
//
// When either of these types of messages is found, it's sent to the tabulator
// for ingesting.
func (t *threader) threadListener(listen *slick.Listener, msg *slick.Message) {
	// if the message is part of a thread or is part of a notification that a thread was updated
	if len(msg.ThreadTimestamp) > 0 || (msg.SubMessage != nil && len(msg.SubMessage.ThreadTimestamp) > 0) {
		t.messagesIn <- msg
	}
}

// Brain is the main serialized data structure for this plugin.  It tracks
// threads by timestamp, messages that are in threads and how long ago a user
// requested thread information.
type Brain struct {
	TsToThread map[string]*Thread
	Messages   []*Message
	UserSeen   map[string]string
}

// Thread is a smaller data structure that contains just the information
// required to track threads
type Thread struct {
	Channel, Text, Ts, LastMsgTs string
}

// Message is a smaller data structure that contains information required to
// track messages that belong to a thread.
type Message struct {
	Channel, Text, Ts, ThreadTs string
}

// tabulator is the heart of the plugin.  It initializes the "brain" data
// structure and then controls access to it by monitoring two channels:
// incoming messages to ingest and incoming summaries to handle.
//
// It also periodically culls out messages that are older than a set cutoff and
// saves the data into the Slick database, so that the bot can be restarted
// without losing all of its data.
func (t *threader) tabulator() {
	var brain Brain

	if err := t.bot.GetDBKey("/threader/brain", &brain); err != nil {
		log.Println("Unable to read brain data, creating new:", err)
		brain = Brain{
			TsToThread: make(map[string]*Thread),
			Messages:   make([]*Message, 0),
			UserSeen:   make(map[string]string),
		}
	}

	saveTimeout := time.After(saveInterval)
	for {
		select {
		case message := <-t.messagesIn:
			if len(message.ThreadTimestamp) > 0 {
				brain.Messages = append(brain.Messages, &Message{
					Channel:  message.FromChannel.Name,
					Text:     message.Text,
					Ts:       message.Timestamp,
					ThreadTs: message.ThreadTimestamp,
				})
			} else if message.SubMessage != nil && len(message.SubMessage.ThreadTimestamp) > 0 {
				if _, ok := brain.TsToThread[message.SubMessage.ThreadTimestamp]; !ok {
					brain.TsToThread[message.SubMessage.ThreadTimestamp] = &Thread{
						Channel:   message.FromChannel.Name,
						Text:      message.SubMessage.Text,
						Ts:        message.SubMessage.ThreadTimestamp,
						LastMsgTs: message.Timestamp,
					}
				} else {
					brain.TsToThread[message.SubMessage.ThreadTimestamp].LastMsgTs = message.Timestamp
				}
			}
		case request := <-t.summaryRequests:
			threadToCount := make(map[*Thread]int)
			// TODO: handle query for date ranges and channels
			if strings.Contains(request.text, " in ") || strings.Contains(request.text, " since ") {
				if !strings.Contains(request.text, " since ") {

				}
			} else {
				for _, mes := range brain.Messages {
					if mes.Ts < request.ts && mes.Ts > brain.UserSeen[request.user] {
						// TODO: check if topic exists in tsToThread
						threadToCount[brain.TsToThread[mes.ThreadTs]]++
					}
				}
			}
			threadSummaries := make([]string, 0)
			for thread, count := range threadToCount {
				threadSummaries = append(threadSummaries, fmt.Sprintf("#%s - %s (%d replies, <https://%s.slack.com/archives/%s/p%s|view>)", thread.Channel, thread.Text, count, t.bot.Config.TeamDomain, thread.Channel, strings.Replace(thread.Ts, ".", "", 1)))
			}
			request.result <- summary{
				count:   len(threadToCount),
				threads: threadSummaries,
			}
			brain.UserSeen[request.user] = request.ts
		case _ = <-saveTimeout:
			cutoff := strconv.FormatInt(time.Now().UTC().Add(-cutoffDays*24*time.Hour).Unix(), 10)

			// throw away any messages older than cutoff
			newMessages := make([]*Message, 0)
			var keep, toss int
			for _, mes := range brain.Messages {
				if mes.Ts > cutoff {
					newMessages = append(newMessages, mes)
					keep++
				} else {
					toss++
				}
			}
			log.Printf("threader === pruned %d and kept %d messages ===\n", toss, keep)
			brain.Messages = newMessages

			// throw away threads older than cutoff
			newTsToThread := make(map[string]*Thread)
			keep = 0
			toss = 0
			for ts, thread := range brain.TsToThread {
				if thread.LastMsgTs > cutoff {
					newTsToThread[ts] = thread
					keep++
				} else {
					toss++
				}
			}
			log.Printf("threader === pruned %d and kept %d threads ===\n", toss, keep)
			brain.TsToThread = newTsToThread

			log.Println("threader === saving brain ===")
			if err := t.bot.PutDBKey("/threader/brain", brain); err != nil {
				log.Println("Error writing brain:", err)
			}

			saveTimeout = time.After(saveInterval)
		}
	}
}
