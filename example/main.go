package main

import (
    "log"
    "github.com/kyprizel/u2fg"
)

type U2FProcessor struct {
}

func (h *U2FProcessor) ProcessMessage(packet []byte) ([]byte, error) {
    log.Printf("in packet: %v", packet)
    return nil, nil
}

func main() {
    h := &U2FProcessor{}
    d, err := u2fg.Init("/dev/hidg0")
    if err != nil {
        log.Fatal(err)
    }
    d.Run(h)
    log.Printf("ok")
    d.Close()
}
