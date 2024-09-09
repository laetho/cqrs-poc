package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

var (
	users = map[string]string{"user1": "password123", "user2": "secret456"}
	kv    nats.KeyValue
	nc    *nats.Conn
)

func main() {
	// Start the embedded NATS server
	ns := startNATSServer()
	defer ns.Shutdown()

	// Connect to the embedded NATS server
	var err error
	nc, err = nats.Connect(ns.ClientURL())
	if err != nil {
		log.Fatal(err)
	}
	defer nc.Close()

	// Setup NATS JetStream and KV
	setupCommandsWQ()
	setupKV(nc)

	processCommands()

	// Define handlers
	http.HandleFunc("/login", loginHandler)
	http.HandleFunc("/logout", logoutHandler)
	http.HandleFunc("/commands", withAuth(commandHandler))
	http.HandleFunc("/queries", withAuth(queryHandler))
	http.HandleFunc("/stream", withAuth(streamHandler)) // SSE

	// Serve static files (HTML)
	http.Handle("/", http.FileServer(http.Dir("./static")))

	// Start server
	log.Println("Server started on :8080")
	http.ListenAndServe(":8080", nil)
}

func startNATSServer() *server.Server {
	opts := &server.Options{
		Host:      "127.0.0.1",
		JetStream: true,
	}
	ns, err := server.NewServer(opts)
	if err != nil {
		log.Fatal(err)
	}

	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		log.Fatal("NATS Server did not start in time")
	}
	return ns
}

func setupKV(nc *nats.Conn) {
	js, err := nc.JetStream()
	if err != nil {
		log.Fatal(err)
	}
	kv, err = js.CreateKeyValue(&nats.KeyValueConfig{Bucket: "sessions"})
	if err != nil {
		log.Fatal(err)
	}
}

func setupCommandsWQ() {
	js, err := nc.JetStream()
	if err != nil {
		log.Fatal(err)
	}

	// Check if the stream already exists
	_, err = js.StreamInfo("COMMAND_STREAM")
	if err == nil {
		log.Println("COMMAND_STREAM already exists")
		return
	}

	// Define the stream configuration
	_, err = js.AddStream(&nats.StreamConfig{
		Name:      "COMMAND_STREAM",
		Subjects:  []string{"commands"}, // Commands will be published to this subject
		Retention: nats.WorkQueuePolicy, // Work queue retention, messages are kept until acknowledged
		Storage:   nats.FileStorage,     // Use file-based storage
		Replicas:  1,                    // Number of replicas (for HA, set to >1)
	})
	if err != nil {
		log.Fatalf("Error creating COMMAND_STREAM: %v", err)
	}
	log.Println("COMMAND_STREAM created successfully")
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	username := r.FormValue("username")
	password := r.FormValue("password")

	if pass, ok := users[username]; ok && pass == password {
		sessionID := generateSessionID()
		kv.Put(sessionID, []byte(username))

		http.SetCookie(w, &http.Cookie{
			Name:     "session_id",
			Value:    sessionID,
			Path:     "/",
			HttpOnly: true,
			Secure:   false,
		})
		w.Write([]byte("Login successful"))
	} else {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
	}
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("session_id")
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	kv.Delete(cookie.Value)

	http.SetCookie(w, &http.Cookie{
		Name:     "session_id",
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   false,
	})
	w.Write([]byte("Logged out successfully"))
}

func withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session_id")
		if err != nil || cookie.Value == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		entry, err := kv.Get(cookie.Value)
		if err != nil {
			http.Error(w, "Session not found or expired", http.StatusUnauthorized)
			return
		}
		fmt.Println(entry)

		next.ServeHTTP(w, r)
	}
}

func commandHandler(w http.ResponseWriter, r *http.Request) {
	sessionID, _ := r.Cookie("session_id")
	command := r.FormValue("command")

	publishToNATS("commands", fmt.Sprintf("%s|%s", sessionID.Value, command))
	w.Write([]byte("Command received!"))
}

func processCommands() {
	js, _ := nc.JetStream()

	// Subscribe to the "commands" subject using JetStream pull-based subscription
	sub, _ := js.PullSubscribe("commands", "command-worker")

	go func() {
		for {
			// Fetch 1 message at a time from the command queue
			msgs, err := sub.Fetch(1)
			if err != nil {
				log.Printf("Error fetching command: %v", err)
				continue
			}

			for _, msg := range msgs {
				// Split the message to extract sessionID and command
				parts := strings.SplitN(string(msg.Data), "|", 2)
				sessionID := parts[0]
				command := parts[1]

				// Simulate processing the command
				result := fmt.Sprintf("Processed command: %s for session: %s", command, sessionID)
				log.Printf("Command processed: %s", result)

				// Publish the result to the "commandResults" subject
				js.Publish("commandResults", []byte(fmt.Sprintf("%s|%s", sessionID, result)))

				// Acknowledge the message so it’s removed from the work queue
				msg.Ack()
			}
		}
	}()
}

func queryHandler(w http.ResponseWriter, r *http.Request) {
	sessionID, _ := r.Cookie("session_id")
	query := r.FormValue("query")

	publishToNATS("queries", fmt.Sprintf("%s|%s", sessionID.Value, query))
	w.Write([]byte("Query received!"))
}

func streamHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sessionID, _ := r.Cookie("session_id")

	sub, _ := nc.Subscribe("commandResults", func(msg *nats.Msg) {
		parts := strings.SplitN(string(msg.Data), "|", 2)
		if parts[0] == sessionID.Value {
			fmt.Fprintf(w, "data: %s\n\n", parts[1])
			w.(http.Flusher).Flush()
		}
	})

	defer sub.Unsubscribe()
	<-r.Context().Done()
}

func generateSessionID() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

func publishToNATS(subject, msg string) {
	nc.Publish(subject, []byte(msg))
}
