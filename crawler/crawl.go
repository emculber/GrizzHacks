package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/lib/pq"
	"golang.org/x/net/html"
)

var config Configuration
var db *sql.DB

const shortForm = "2006-Jan-02"

const URL = "http://finance.yahoo.com/q/h?s=%s&t=%s"

type databaseInfo struct {
	Host     string
	Port     int
	Username string
	Password string
	Dbname   string
}

type Configuration struct {
	Db databaseInfo
}

type Article struct {
	Id            int
	PublishedDate string
	RawArticle    string
	ParsedArticle string
	Ticker        string
}

type Data struct {
	Ticker string
	Date   string
	Urls   []string
	Url    string
}

func init() {
	file, err := os.Open("config.json")
	if err != nil {
		panic(err)
	}

	decoder := json.NewDecoder(file)
	err = decoder.Decode(&config)
	if err != nil {
		panic(err)
	}

	dbUrl := fmt.Sprintf("postgres://%s:%s@%s/%s", config.Db.Username, config.Db.Password, config.Db.Host, config.Db.Dbname)
	db, err = sql.Open("postgres", dbUrl)
	if err != nil {
		panic(err)
	}
}

func workerpoolGetPageBodies(i int, jobs <-chan Data, results chan<- bool) {
	for data := range jobs {
		fmt.Printf("%d working on %s\n", i, data)
		getPageAndStore(data.Url, data.Date, data.Ticker)
		results <- true
	}
}

func workerpoolGetUrlsToGrab(i int, jobs <-chan Data, results chan<- Data) {
	for data := range jobs {
		fmt.Printf("%d working on %s\n", i, data)
		urls, err := getLinks(data)
		if err != nil {
			results <- data
			continue
		}

		data.Urls = urls
		results <- data
	}
}

func getAllTickers() []string {
	rows, err := db.Query("select upper(ticker) from tickers")
	if err != nil {
		panic(err)
	}

	var tickers []string

	for rows.Next() {
		var ticker string
		if err = rows.Scan(&ticker); err != nil {
			panic(err)
		}
		tickers = append(tickers, ticker)
	}

	return tickers
}

func getDates() []string {
	var dates []string

	start, _ := time.Parse(shortForm, "2016-Jan-03")
	end, _ := time.Parse(shortForm, "2016-Mar-19")

	for current := start; !current.Equal(end); current = current.AddDate(0, 0, 1) {
		if current.Weekday() == 0 || current.Weekday() == 6 {
			continue
		}

		date := fmt.Sprintf("%d-%d-%d", current.Year(), current.Month(), current.Day())
		dates = append(dates, date)
	}

	return dates
}

func getLinks(data Data) ([]string, error) {
	var urls []string

	url := fmt.Sprintf(URL, data.Ticker, data.Date)

	resp, err := http.Get(url)
	defer resp.Body.Close()
	if err != nil {
		return urls, err
	}

	if resp.StatusCode != 200 {
		fmt.Printf("Cannot get %s\n", url)
		return urls, errors.New("Bad")
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return urls, err
	}

	page := string(body)

	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return urls, err
	}

	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "table" {
			tbody := n.FirstChild
			for tr := tbody.FirstChild; tr != nil; tr = tr.NextSibling {
				td := tr.FirstChild
				if td.Type == html.ElementNode && td.Data == "td" {
					div := td.FirstChild
					if div == nil {
						break
					}

					if div.Type == html.ElementNode && div.Data == "div" {
						for c := div.FirstChild; c != nil; c = c.NextSibling {
							if c.Type == html.ElementNode && c.Data == "ul" {
								for li := c.FirstChild; li != nil; li = li.NextSibling {
									if li.FirstChild.Type == html.ElementNode && li.FirstChild.Data == "a" {
										for _, a := range li.FirstChild.Attr {
											urls = append(urls, a.Val)
										}
									}
								}
							}
						}
					}
				}
			}
		}

		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(doc)

	return urls, nil
}

func getPageAndStore(url, date, ticker string) {
	resp, err := http.Get(url)
	defer resp.Body.Close()
	if err != nil {
		return
	}

	if resp.StatusCode != 200 {
		fmt.Printf("Cannot get %s\n", url)
		return
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(err)
		return
	}

	txn, err := db.Begin()
	if err != nil {
		fmt.Println(err)
		return
	}

	stmt, err := txn.Prepare(pq.CopyIn("articles", "url", "pubdate", "ticker", "raw"))
	if err != nil {
		fmt.Println(err)
		return
	}

	d, _ := time.Parse("2006-2-06", date)
	fmt.Println(date)
	fmt.Println(d.Unix())

	_, err = stmt.Exec(url, d.Unix(), ticker, string(body))
	if err != nil {
		fmt.Println(err)
		return
	}

	err = stmt.Close()
	if err != nil {
		fmt.Println(err)
		return
	}

	err = txn.Commit()
	if err != nil {
		fmt.Println(err)
		return
	}
}

func main() {
	jobs := make(chan Data, 100)
	results := make(chan Data, 100)

	for w := 0; w < 4; w++ {
		go workerpoolGetUrlsToGrab(w, jobs, results)
	}

	var datas []Data

	for _, ticker := range getAllTickers() {
		for _, date := range getDates() {
			var data Data
			jobs <- Data{ticker, date, nil, ""}
			data = <-results
			datas = append(datas, data)
		}
	}

	jobs2 := make(chan Data, 100)
	results2 := make(chan bool, 100)

	for w := 0; w < 4; w++ {
		go workerpoolGetPageBodies(w, jobs2, results2)
	}

	for _, data := range datas {
		for _, url := range data.Urls {
			jobs2 <- Data{data.Ticker, data.Date, nil, url}
			<-results2
		}
	}
}
