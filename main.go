package main

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/anacrolix/dms/dlna/dms"
	"github.com/anacrolix/log"
	"github.com/judwhite/go-svc"
)

type program struct {
	dmsServer *dms.Server
	cache     *fFprobeCache
	logger    log.Logger
	wg        sync.WaitGroup
	quit      chan struct{}
}

func main() {
	if err := svc.Run(&program{logger: log.Default.WithNames("main")}); err != nil {
		log.Fatal(err)
	}
}

func (p *program) Init(env svc.Environment) error {
	if env.IsWindowsService() {
		dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
		if err != nil {
			return err
		}

		logPath := filepath.Join(dir, "dmssvc.log")

		f, err := os.OpenFile(logPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
		if err != nil {
			return err
		}

		p.logger.Handlers = []log.Handler{
			log.StreamHandler{
				W:   f,
				Fmt: log.LineFormatter,
			},
		}
		if err := config.load(filepath.Join(dir, "dmssvc.json")); err != nil {
			p.logger.Print(err)
		}
	}

	return nil
}

func (p *program) Start() error {
	p.quit = make(chan struct{})

	p.wg.Add(1)
	go func() {
		log.Println("Starting...")
		go p.run()
		<-p.quit
		log.Println("Quit signal received...")
		p.wg.Done()
	}()

	return nil
}

func (p *program) Stop() error {
	p.logger.Println("Stopping...")
	err := p.dmsServer.Close()
	if err != nil {
		log.Fatal(err)
	}
	if err := p.cache.save(config.FFprobeCachePath); err != nil {
		p.logger.Print(err)
	}
	close(p.quit)
	p.wg.Wait()
	p.logger.Println("Stopped.")
	return nil
}
