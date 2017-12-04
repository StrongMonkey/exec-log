package main

import (
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

func main() {
	url := "ws://localhost:9001/exec?pod=nginx-4217019353-npcjr&namespace=default&stdin=true&stdout=true&stderr=true&command=bash"
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		logrus.Fatal(err)
	}
	defer c.Close()

	go func() {
		defer c.Close()
		for {
			msgType, message, err := c.ReadMessage()
			if err != nil {
				logrus.Errorf("read: %v", err)
				return
			}
			if msgType == websocket.CloseMessage {
				logrus.Fatalf("Receiving close ping. Closing websocket connection")
			}
			logrus.Infof("recv: %s", message)
		}
	}()

	err = c.WriteMessage(websocket.TextMessage, []byte("echo hello\n"))
	if err != nil {
		logrus.Fatal(err)
	}
	select {}
}
