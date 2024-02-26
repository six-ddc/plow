package main

import (
	"flag"
	"log"
	"math/rand"
	"net/http"
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
		statusCodes := []int{
			http.StatusOK, http.StatusOK, http.StatusBadRequest, http.StatusTooManyRequests, http.StatusBadGateway,
		}
		c.SetStatusCode(statusCodes[rand.Intn(len(statusCodes))])
		_, werr := c.Write(c.Request.Body())
		if werr != nil {
			log.Println(werr)
		}
	}))
}
