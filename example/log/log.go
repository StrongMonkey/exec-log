package main

import (
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

func main() {
	url := "ws://localhost:9001/log?pod=test1-3986794105-wq7tm&namespace=default&timestamp=true&follow=true"
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
				logrus.Fatalf("read: %v", err)
				return
			}
			if msgType == websocket.CloseMessage {
				logrus.Fatalf("Receiving close ping. Closing websocket connection")
			}
			logrus.Infof("recv: %s", message)
		}
	}()
	select {}
}
