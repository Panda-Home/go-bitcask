package server

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Panda-Home/bitcask/bitlog"
	"github.com/Panda-Home/bitcask/config"
	"github.com/Panda-Home/bitcask/data"
)

var (
	errEmptyCommand   = errors.New("") // say nothing when given empty command
	errUnknownCommand = errors.New("Unknown command")
	errTooManyArgs    = errors.New("Too many arguments")
	errTooFewArgs     = errors.New("Too few arguments")
)

type Server struct {
	listener net.Listener
	running  bool
	quit     chan interface{}
	logFile  *bitlog.Logger
	keyDir   *data.KeyDir

	mu sync.Mutex
	wg sync.WaitGroup
}

func NewServer(c *config.BitcaskConfig) (*Server, error) {
	addr := fmt.Sprintf("%s:%d", c.Host, c.Port)
	s := &Server{
		quit: make(chan interface{}),
	}
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("Cannot resolve address: %s", addr)
	}
	l, err := net.Listen("tcp", tcpAddr.String())
	if err != nil {
		log.Fatal(err)
	}
	s.listener = l

	s.keyDir = data.NewKeyDir() // in-memory structure initialization
	logFile, err := bitlog.NewLogger(c.DataDir, c.DataSize)
	if err != nil {
		log.Fatal(err)
	}
	s.logFile = logFile
	s.loadExistingLog()

	s.wg.Add(1)
	log.Printf("Listening on %v\n", addr)
	go s.serve()
	return s, nil
}

func (s *Server) Stop() {
	close(s.quit)
	s.listener.Close()
	s.logFile.Close()
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()
	s.wg.Wait()
}

// Build KeyDir structure from existing log files
func (s *Server) loadExistingLog() error {
	files, err := ioutil.ReadDir(s.logFile.Dirpath)
	if err != nil {
		return err
	}

	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if !strings.HasPrefix(f.Name(), "data.bit.") {
			continue
		}
		filePath := filepath.Join(s.logFile.Dirpath, f.Name())
		fileHandler, err := os.OpenFile(filePath, os.O_RDONLY, 0644)
		if err != nil {
			log.Printf("Failed to open file: %s", filePath)
			continue
		}

		// Read each record from file to build KeyDir
		var curPos int64 = 0
		for {
			entry, err := data.LoadFromFile(fileHandler, curPos)
			if err != nil {
				break
			}
			valueSize := entry.ValueSize
			key := entry.Key
			if valueSize > 0 {
				// not a delete record
				s.keyDir.SetEntryFromKeyValue(key, filePath, curPos, valueSize, entry.Timestamp)
			} else {
				s.keyDir.DelKeydirEntry(key)
			}
			curPos += int64(160 + entry.KeySize + valueSize)
		}
		fileHandler.Close()
	}
	return nil
}

func (s *Server) serve() {
	defer s.wg.Done()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return
			default:
				log.Println("accept error", err)
			}
		} else {
			s.wg.Add(1)
			go func() {
				s.handleConection(conn)
				s.wg.Done()
			}()
		}
	}
}

func (s *Server) handleConection(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 4096)
ReadLoop:
	for {
		select {
		case <-s.quit:
			return
		default:
			conn.SetDeadline(time.Now().Add(200 * time.Millisecond))
			n, err := conn.Read(buf)
			if err != nil {
				if opErr, ok := err.(*net.OpError); ok && opErr.Timeout() {
					continue ReadLoop
				} else if err != io.EOF {
					log.Println("read error", err)
					return
				}
			}
			if n == 0 {
				return
			}
			log.Printf("received from %v: %s", conn.RemoteAddr(), string(buf[:n]))
			result, err := s.processCommand(strings.TrimSpace(string(buf[:n])))
			if err != nil {
				conn.Write([]byte(err.Error()))
			} else {
				conn.Write(result)
			}

		}
	}
}

func (s *Server) processCommand(cmd string) ([]byte, error) {
	if len(cmd) == 0 {
		return nil, errEmptyCommand
	}
	tokens := strings.Fields(cmd)
	switch tokens[0] {
	case "set":
		if len(tokens) > 3 {
			return nil, errTooManyArgs
		}
		if len(tokens) < 3 {
			return nil, errTooFewArgs
		}
		if err := s.Set([]byte(tokens[1]), []byte(tokens[2])); err != nil {
			return nil, err
		}
		return []byte("OK"), nil
	case "get":
		if len(tokens) > 2 {
			return nil, errTooManyArgs
		}
		if len(tokens) < 2 {
			return nil, errTooFewArgs
		}
		value, err := s.Get([]byte(tokens[1]))
		if err != nil {
			return nil, err
		}
		return value, nil
	case "del":
		if len(tokens) > 2 {
			return nil, errTooManyArgs
		}
		if len(tokens) < 2 {
			return nil, errTooFewArgs
		}
		err := s.Del([]byte(tokens[1]))
		if err != nil {
			return nil, err
		}
		return []byte("OK"), nil
	default:
		return nil, errUnknownCommand
	}
}

// IsRunning returns the server running status
func (s *Server) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}
