package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/gorilla/mux"
)

type LinkResolverResponse struct {
	Status  int    `json:"status"`
	Message string `json:"message,omitempty"`

	Tooltip string `json:"tooltip,omitempty"`
	Link    string `json:"link,omitempty"`

	// Flag in the BTTV API to.. maybe signify that the link will download something? idk
	// Download *bool  `json:"download,omitempty"`
}

var noLinkInfoFound = &LinkResolverResponse{
	Status:  404,
	Message: "No link info found",
}

var invalidURL = &LinkResolverResponse{
	Status:  500,
	Message: "Invalid URL",
}

func unescapeURLArgument(r *http.Request, key string) (string, error) {
	vars := mux.Vars(r)
	escapedURL := vars[key]
	url, err := url.PathUnescape(escapedURL)
	if err != nil {
		return "", err
	}

	return url, nil
}

func formatDuration(dur string) string {
	dur = strings.ToLower(dur)
	dur = strings.Replace(dur, "pt", "", 1)
	d, _ := time.ParseDuration(dur)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

func insertCommas(str string, n int) string {
	var buffer bytes.Buffer
	var remainder = n - 1
	var lenght = len(str) - 2
	for i, rune := range str {
		buffer.WriteRune(rune)
		if (lenght-i)%n == remainder {
			buffer.WriteRune(',')
		}
	}
	return buffer.String()
}

var linkResolverRequestsMutex sync.Mutex
var linkResolverRequests = make(map[string][](chan interface{}))

type customURLManager struct {
	check func(resp *http.Response) bool
	run   func(resp *http.Response) ([]byte, error)
}

var (
	customURLManagers []customURLManager
)

func doRequest(url string) {
	response := cacheGetOrSet("url:"+url, 10*time.Minute, func() (interface{}, error) {
		resp, err := client.Get(url)
		if err != nil {
			if strings.HasSuffix(err.Error(), "no such host") {
				return json.Marshal(noLinkInfoFound)
			}

			return json.Marshal(&LinkResolverResponse{Status: 500, Message: "client.Get " + err.Error()})
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
			doc, err := goquery.NewDocumentFromReader(resp.Body)
			if err != nil {
				return json.Marshal(&LinkResolverResponse{Status: 500, Message: "html parser error " + err.Error()})
			}
			if strings.HasSuffix(resp.Request.URL.Host, ".youtube.com") {
				// do special youtube parsing

				url := resp.Request.URL
				videoID := ""

				if strings.Index(url.Path, "embed") == -1 {
					videoID = url.Query().Get("v")
				} else {
					videoID = path.Base(url.Path)
				}

				if videoID == "" {
					return json.Marshal(noLinkInfoFound)
				}

				youtubeResponse := cacheGetOrSet("youtube:"+videoID, 1*time.Hour, func() (interface{}, error) {
					video, err := getYoutubeVideo(videoID)
					if err != nil {
						return &LinkResolverResponse{Status: 500, Message: "youtube api error " + err.Error()}, nil
					}

					fmt.Println("Doing YouTube API Request on", videoID)
					return &LinkResolverResponse{
						Status: resp.StatusCode,
						Tooltip: "<div style=\"text-align: left;\"><b>" + html.EscapeString(video.Snippet.Title) +
							"</b><hr><b>Channel:</b> " + html.EscapeString(video.Snippet.ChannelTitle) +
							"<br><b>Duration:</b> " + html.EscapeString(formatDuration(video.ContentDetails.Duration)) +
							"<br><b>Views:</b> " + insertCommas(strconv.FormatUint(video.Statistics.ViewCount, 10), 3) +
							"<br><b>Likes:</b> <span style=\"color: green;\">+" + insertCommas(strconv.FormatUint(video.Statistics.LikeCount, 10), 3) +
							"</span>/<span style=\"color: red;\">-" + insertCommas(strconv.FormatUint(video.Statistics.DislikeCount, 10), 3) +
							"</span></div>",
					}, nil
				})

				return json.Marshal(youtubeResponse)
			}

			for _, m := range customURLManagers {
				if m.check(resp) {
					return m.run(resp)
				}
			}

			escapedTitle := doc.Find("title").First().Text()
			if escapedTitle != "" {
				escapedTitle = fmt.Sprintf("<b>%s</b><hr>", html.EscapeString(escapedTitle))
			}
			return json.Marshal(&LinkResolverResponse{
				Status:  resp.StatusCode,
				Tooltip: fmt.Sprintf("<div style=\"text-align: left;\">%s<b>URL:</b> %s</div>", escapedTitle, html.EscapeString(resp.Request.URL.String())),
				Link:    resp.Request.URL.String(),
			})
		}

		return json.Marshal(noLinkInfoFound)
	})

	linkResolverRequestsMutex.Lock()
	fmt.Println("Notify channels")
	for _, channel := range linkResolverRequests[url] {
		fmt.Printf("Notify channel %v\n", channel)
		/*
			select {
			case channel <- response:
				fmt.Println("hehe")
			default:
				fmt.Println("Unable to respond")
			}
		*/
		channel <- response
	}
	delete(linkResolverRequests, url)
	linkResolverRequestsMutex.Unlock()
}

func linkResolver(w http.ResponseWriter, r *http.Request) {
	url, err := unescapeURLArgument(r, "url")
	if err != nil {
		bytes, err := json.Marshal(invalidURL)
		if err != nil {
			fmt.Println("Error marshalling invalidURL struct:", err)
			return
		}
		_, err = w.Write(bytes)
		if err != nil {
			fmt.Println("Error in w.Write:", err)
		}
		return
	}

	cacheKey := "url:" + url

	var response interface{}

	if data := cacheGet(cacheKey); data != nil {
		response = data
	} else {
		responseChannel := make(chan interface{})

		linkResolverRequestsMutex.Lock()
		linkResolverRequests[url] = append(linkResolverRequests[url], responseChannel)
		urlRequestsLength := len(linkResolverRequests[url])
		linkResolverRequestsMutex.Unlock()
		if urlRequestsLength == 1 {
			// First poll for this URL, start the request!
			go doRequest(url)
		}

		fmt.Printf("Listening to channel %v\n", responseChannel)
		response = <-responseChannel
		fmt.Println("got response!")
	}

	_, err = w.Write(response.([]byte))
	if err != nil {
		fmt.Println("Error in w.Write:", err)
	}
}
