package main

import (
	"flag"
	"log"
	"math/rand"
	"strconv"
	"time"

	"github.com/valyala/fasthttp"
)

var serverPort = flag.Int("p", 8080, "port to use for benchmarks")

func main() {
	flag.Parse()
	rand.Seed(time.Now().UnixNano())
	addr := "localhost:" + strconv.Itoa(*serverPort)
	log.Println("Starting HTTP server on:", addr)
	log.Fatalln(fasthttp.ListenAndServe(addr, func(c *fasthttp.RequestCtx) {
		//time.Sleep(time.Duration(rand.Int63n(int64(5 * time.Second))))
		if rand.Intn(5) == 0 {
			c.SetStatusCode(400)
		}
		_, werr := c.Write(c.Request.Body())
		if werr != nil {
			log.Println(werr)
		}
	}))
}
