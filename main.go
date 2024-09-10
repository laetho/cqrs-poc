package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

var (
	users  = map[string]string{"user1": "password123", "user2": "secret456"}
	kv     jetstream.KeyValue
	nc     *nats.Conn
	ctx    context.Context
	cancel context.CancelFunc
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
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
	setupQueriesWQ()
	setupKV(nc)

	processCommands()
	processQueries()

	// Define handlers
	http.HandleFunc("/login", loginHandler)
	http.HandleFunc("/logout", logoutHandler)
	http.HandleFunc("/commands", withAuth(commandHandler))
	http.HandleFunc("/queries", withAuth(queryHandler))
	http.HandleFunc("/stream/commands", withAuth(streamHandler)) // SSE
	http.HandleFunc("/stream/queries", withAuth(queriesHandler)) // SSE

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
		StoreDir:  "./data",
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
	js, err := jetstream.New(nc)
	if err != nil {
		log.Fatal(err)
	}

	kv, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "sessions"})
	if err != nil {
		log.Fatal(err)
	}
}

func setupCommandsWQ() {
	js, err := jetstream.New(nc)
	if err != nil {
		log.Fatal(err)
	}

	s, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      "wq-commands",
		Subjects:  []string{"commands"},      // Commands will be published to this subject
		Retention: jetstream.WorkQueuePolicy, // Work queue retention, messages are kept until acknowledged
		Storage:   jetstream.MemoryStorage,   // Use memory-based storage
		Replicas:  1,                         // Number of replicas (for HA, set to >1)
	})
	if err != nil {
		log.Fatalf("Error creating COMMAND_STREAM: %v", err)
	}
	log.Println(s.CachedInfo().Config.Name, "created successfully")
}

func setupQueriesWQ() {
	js, err := jetstream.New(nc)
	if err != nil {
		log.Fatal(err)
	}

	s, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      "wq-queries",
		Subjects:  []string{"queries"},       // Commands will be published to this subject
		Retention: jetstream.WorkQueuePolicy, // Work queue retention, messages are kept until acknowledged
		Storage:   jetstream.MemoryStorage,   // Use memory-based storage
		Replicas:  1,                         // Number of replicas (for HA, set to >1)
	})
	if err != nil {
		log.Fatalf("Error creating COMMAND_STREAM: %v", err)
	}
	log.Println(s.CachedInfo().Config.Name, "created successfully")
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	username := r.FormValue("username")
	password := r.FormValue("password")

	if pass, ok := users[username]; ok && pass == password {
		sessionID := generateSessionID()
		id, err := kv.Put(ctx, sessionID, []byte(username))
		if err != nil {
			log.Fatal(err)
		}
		log.Print("Session ID: ", id)

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

	kv.Delete(ctx, cookie.Value)

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

		_, err = kv.Get(ctx, cookie.Value)
		if err != nil {
			http.Error(w, "Session not found or expired", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	}
}

func commandHandler(w http.ResponseWriter, r *http.Request) {
	sessionID, _ := r.Cookie("session_id")
	command := r.FormValue("command")

	publishToNATS("commands", fmt.Sprintf("%s|%s", sessionID.Value, command))
	w.Write([]byte("Command received!"))
	fmt.Println("got command")
}

func processCommands() {
	js, _ := jetstream.New(nc)

	cons, err := js.CreateOrUpdateConsumer(ctx, "wq-commands", jetstream.ConsumerConfig{
		Durable:   "commands-woker",
		AckPolicy: jetstream.AckExplicitPolicy,
	})
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		// Fetch 1 message at a time from the command queue
		_, err := cons.Consume(func(msg jetstream.Msg) {
			parts := strings.SplitN(string(msg.Data()), "|", 2)
			sessionID := parts[0]
			command := parts[1]

			// Simulate processing the command
			result := fmt.Sprintf("Processed command: %s for session: %s", command, sessionID)
			log.Printf("Command processed: %s", result)

			// Publish the result to the "commandResults" subject
			nc.Publish("commandResults", []byte(fmt.Sprintf("%s|%s", sessionID, result)))

			// Acknowledge the message so it’s removed from the work queue
			msg.Ack()
		}, jetstream.ConsumeErrHandler(func(consumeCtx jetstream.ConsumeContext, err error) {
			log.Printf("Error consuming message: %v", err)
		}))
		if err != nil {
			log.Fatal(err)
		}
	}()
}

func processQueries() {
	js, _ := jetstream.New(nc)

	cons, err := js.CreateOrUpdateConsumer(ctx, "wq-queries", jetstream.ConsumerConfig{
		Durable:   "queries-woker",
		AckPolicy: jetstream.AckExplicitPolicy,
	})
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		// Fetch 1 message at a time from the command queue
		_, err := cons.Consume(func(msg jetstream.Msg) {
			parts := strings.SplitN(string(msg.Data()), "|", 2)
			sessionID := parts[0]
			command := parts[1]

			// Simulate processing the command
			result := fmt.Sprintf("Processed query %s for session: %s", command, sessionID)
			log.Printf("Command processed: %s", result)

			// Publish the result to the "commandResults" subject
			nc.Publish("queryResults", []byte(fmt.Sprintf("%s|%s", sessionID, result)))

			// Acknowledge the message so it’s removed from the work queue
			msg.Ack()
		}, jetstream.ConsumeErrHandler(func(consumeCtx jetstream.ConsumeContext, err error) {
			log.Printf("Error consuming message: %v", err)
		}))
		if err != nil {
			log.Fatal(err)
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

func queriesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sessionID, _ := r.Cookie("session_id")

	sub, _ := nc.Subscribe("queryResults", func(msg *nats.Msg) {
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
	err := nc.Publish(subject, []byte(msg))
	if err != nil {
		log.Printf("Error publishing message: %v", err)
	}
}
