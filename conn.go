/*
Copyright (C) 2020  Branden J Brown

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"github.com/AbsyntheSyne/robot/irc"
)

// contextDialer is typically either *net.Dialer or *tls.Dialer.
type contextDialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

type connectConfig struct {
	dialer  contextDialer
	addr    string // format accepted by DialContext
	retries []time.Duration
	nick    string // also used for user
	pass    string
	timeout time.Duration
}

// connect connects to an IRC server. It should be used in a go statement. Once
// the connection is finished, connect closes recv.
//
// connect automatically handles reconnecting, whether due to net errors or
// RECONNECT messages from the server. To disconnect and not reconnect, send a
// QUIT message, or close the context; in the latter case, connect will
// automatically send a QUIT to the server. Additionally, as a special case,
// sending a RECONNECT message closes the connection and reconnects.
func connect(ctx context.Context, config connectConfig, send <-chan irc.Message, recv chan<- irc.Message, lg *log.Logger) {
	pctx, cancel := context.WithCancel(ctx)
	sem := make(chan struct{}, 2)
	for pctx.Err() == nil {
		lg.Println("connecting to", config.addr)
		conn, err := config.dialer.DialContext(ctx, "tcp", config.addr)
		if err != nil {
			lg.Println("connection error:", err)
			for _, wait := range config.retries {
				time.Sleep(wait)
				conn, err = config.dialer.DialContext(ctx, "tcp", config.addr)
				if err != nil {
					lg.Println("connection error:", err)
					continue
				}
				break
			}
			if err != nil {
				lg.Println("out of retries, giving up")
				break
			}
		}
		ppctx, pcancel := context.WithCancel(pctx)
		go connSender(ppctx, cancel, config, send, sem, conn, lg)
		go connRecver(ppctx, pcancel, config, recv, sem, conn, lg)
		select {
		case <-ctx.Done():
			// Context closed. Close the connection so the reader and writer
			// unblock, then receive a value from the semaphore in place of the
			// one we'd normally receive on the other case.
			conn.Close()
			<-sem
		case <-sem: // do nothing
		}
		// Repeat of the same select for the same reasons. We might double-,
		// triple-, maybe even quadruple-close the connection, but that's ok.
		select {
		case <-ctx.Done():
			conn.Close()
			<-sem
		case <-sem: // do nothing
		}
	}
	cancel()
	close(recv)
}

func connSender(ctx context.Context, cancel context.CancelFunc, config connectConfig, send <-chan irc.Message, sem chan struct{}, conn net.Conn, lg *log.Logger) {
	defer func() { sem <- struct{}{} }()
	defer conn.Close()
	write := func(msg string) error {
		lg.Println("send:", msg)
		conn.SetWriteDeadline(time.Now().Add(config.timeout))
		_, err := io.WriteString(conn, msg+"\r\n")
		return err
	}
	li := fmt.Sprintf("CAP REQ :twitch.tv/commands twitch.tv/tags\r\nPASS %[2]s\r\nNICK %[1]s\r\nUSER %[1]s", config.nick, config.pass)
	if err := write(li); err != nil {
		lg.Println("error while writing:", err)
		conn.Close()
		return
	}
	for {
		select {
		case <-ctx.Done():
			lg.Println("sender: context closed")
			go write("QUIT :goodbye") // error doesn't matter
			return
		case msg, ok := <-send:
			if !ok {
				cancel()
				lg.Println("sender: message channel closed")
				go write("QUIT :goodbye") // error doesn't matter
				return
			}
			switch msg.Command {
			case "":
				// do nothing, ignore zero values
			case "QUIT":
				cancel()
				write(msg.String()) // error doesn't matter
				return
			case "RECONNECT":
				write("QUIT :goodbye") // error doesn't matter
				return
			case "PRIVMSG":
				// Check that the message is ok to send.
				if badmatch(msg) {
					lg.Println("blocked", msg)
					continue
				}
				fallthrough
			default:
				err := write(msg.String())
				if err != nil {
					lg.Println("error while writing:", err)
					conn.Close()
					return
				}
			}
		}
	}
}

func connRecver(ctx context.Context, cancel context.CancelFunc, config connectConfig, recv chan<- irc.Message, sem chan struct{}, conn net.Conn, lg *log.Logger) {
	defer func() { sem <- struct{}{} }()
	defer cancel()
	r := bufio.NewReaderSize(conn, 8192+512+2)
	for {
		conn.SetReadDeadline(time.Now().Add(config.timeout))
		msg, err := irc.Parse(r)
		if err != nil {
			lg.Printf("error while recving: %v (got msg %#v)", err, msg)
			if _, ok := err.(irc.Malformed); ok {
				continue
			}
			conn.Close()
			return
		}
		switch msg.Command {
		case "RECONNECT":
			lg.Println("recver: got RECONNECT, closing connection")
			conn.Close()
			return
		case "PING":
			conn.SetWriteDeadline(time.Now().Add(config.timeout))
			_, err := io.WriteString(conn, "PONG :"+msg.Trailing+"\r\n")
			if err != nil {
				lg.Println("error while sending PONG:", err)
				conn.Close()
				return
			}
			// Check the context for cancellation.
			if ctx.Err() != nil {
				lg.Println("recver: context closed")
				// sender handles disconnecting in this case
				return
			}
			continue
		default:
			lg.Println("recv:", msg.String())
			select {
			case <-ctx.Done():
				lg.Println("recver: context closed")
				return
			case recv <- msg:
				// do nothing
			}
		}
	}
}
