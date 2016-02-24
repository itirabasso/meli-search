// Este programa es un buscador de artículos en Mercadolibre. Por ahora solo busca bicicletas "kona".
// La idea es ir marcando las bicicletas que ya fueron vistas y esto cada un minuto va consultando por nuevas bicicletas usando el api de mercadolibre.
// Se levanta un servidor http que maneja los requests, en el puerto 8080 hay dos endpoints:
// /listing que muestra todas las publicaciones que matchearon
// /visited que marca una publicacion como visitada (y la excluye del proximo listado).

// TODO: Tal vez convenga listar los artículos en una tabla, usar algo como bootstrap table que es lindo.
package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type ApiResponse struct {
	Query   string
	Paging  paging
	Results []result
}

type paging struct {
	Total  int
	Offset int
	Limit  int
}

type result struct {
	Id        string
	Permalink string
	Thumbnail string
	Title     string
	Price     float64
}

type Query struct {
	sync.RWMutex
	Endpoint  string
	Params    map[string]string
	Available map[string]result
	Visited   map[string]result
}

var queries map[string]*Query

// meliSearch does a Mercado Libre Search with the given params, returning an ApiResponse.
// This search automatically tries to get all the results (requesting all the available pages).
func meliSearch(params map[string]string) (rs []result, err error) {
	const hostname string = "https://api.mercadolibre.com/sites/MLA/search"
	const elemsPerPage string = "1000" // max amount of elems per page, 200 is the limit.

	u, err := url.Parse(hostname)
	if err != nil {
		return nil, fmt.Errorf("could not parse url: %v", err)
	}

	query := u.Query()
	for k, v := range params {
		query.Set(k, v)
	}

	query.Set("limit", elemsPerPage)
	u.RawQuery = query.Encode()

	// Get First Page.
	var resp *http.Response
	for {
		resp, err = http.Get(u.String())
		if err != nil {
			log.Printf("could not make request: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		if resp.StatusCode >= 300 {
			resp.Body.Close()
			log.Printf("unexpected status code: %q", resp.Status)
			time.Sleep(5 * time.Second)
			continue
		}
		break
	}

	searchResp := &ApiResponse{}
	decoder := json.NewDecoder(resp.Body)
	decoder.Decode(searchResp)
	resp.Body.Close()

	rs = append(rs, searchResp.Results...)

	for searchResp.Paging.Offset+searchResp.Paging.Limit < searchResp.Paging.Total {
		time.Sleep(time.Second / 2)
		log.Printf("Fetching offset: %d\ttotal: %d", searchResp.Paging.Offset+searchResp.Paging.Limit, searchResp.Paging.Total)
		query.Set("offset", fmt.Sprintf("%d", searchResp.Paging.Offset+searchResp.Paging.Limit))

		u.RawQuery = query.Encode()
		resp, err = http.Get(u.String())
		if err != nil {
			log.Printf("could not make request: %v. Retrying in 5 seconds.", err)
			time.Sleep(5 * time.Second)
			continue
		}
		if resp.StatusCode >= 300 {
			resp.Body.Close()
			log.Printf("unexpected status code: %q. Retrying in 5 seconds.", err)
			time.Sleep(5 * time.Second)
			continue
		}

		searchResp = &ApiResponse{}
		decoder = json.NewDecoder(resp.Body)
		decoder.Decode(searchResp)
		resp.Body.Close()

		rs = append(rs, searchResp.Results...)
	}

	return rs, nil
}

func updateQuery(q *Query) {
	rs, err := meliSearch(q.Params)
	if err != nil {
		log.Fatal(err)
	}

	newResults := map[string]result{}
	q.Lock()
	for _, v := range rs {
		if _, ok := q.Visited[v.Id]; ok {
			continue
		}

		// Esto es un hack horrible para hacer las imagenes más grandes.
		// Resulta que la respuesta de ML te pone los thumbnails chiquitos, pero al final de la url
		// hay una imagen más grande si cambias el I por una Y. (puramente experimental).
		v.Thumbnail = strings.Replace(v.Thumbnail, "-I.jpg", "-U.jpg", -1)

		newResults[v.Id] = v
	}
	q.Available = newResults
	log.Printf("available: %d", len(q.Available))
	q.Unlock()

}

func main() {

	f, err := os.Open("query.db")
	if err != nil {
		log.Fatalf("failed to open query db: %v", err)
	}

	json.NewDecoder(f).Decode(&queries)
	f.Close()

	log.Println(queries)
	for endpoint, query := range queries {
		log.Printf("Starting process for query %q", endpoint)
		if query.Available == nil {
			query.Available = map[string]result{}
		}
		if query.Visited == nil {
			query.Visited = map[string]result{}
		}
		go func(query *Query) {
			updateQuery(query)
			c := time.Tick(25 * time.Minute)
			for _ = range c {
				updateQuery(query)
			}
		}(query)
	}

	go func() {
		for _ = range time.Tick(10 * time.Minute) {
			log.Println("Backup time!")
			for _, query := range queries {
				query.RLock()
			}

			// once we have all the locks, we can serialize the structure
			f, err := os.Create("query.db.new")
			if err != nil {
				log.Fatal(err)
			}
			json.NewEncoder(f).Encode(queries)
			f.Close()
			os.Rename("query.db.new", "query.db")

			for _, query := range queries {
				query.RUnlock()
			}
			log.Println("Backup Done!")
		}
	}()

	http.HandleFunc("/visited", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		endpoint := r.URL.Query().Get("endpoint")

		if id == "" || endpoint == "" {
			return
		}
		log.Printf("Deleting %q from endpoint %q", id, endpoint)

		defer func() {
			w.Write([]byte(id))
			// http.Redirect(w, r, "/listing?endpoint="+endpoint, http.StatusFound)
		}()
		queries[endpoint].Lock()
		defer queries[endpoint].Unlock()
		v, ok := queries[endpoint].Available[id]
		if !ok {
			return
		}
		queries[endpoint].Visited[id] = v
		delete(queries[endpoint].Available, id)
	})
	http.HandleFunc("/visitedAll", func(w http.ResponseWriter, r *http.Request) {
		endpoint := r.URL.Query().Get("endpoint")
		if endpoint == "" {
			return
		}
		defer func() {
			http.Redirect(w, r, "/listing?endpoint="+endpoint, http.StatusFound)
		}()

		ids, ok := r.URL.Query()["id"]
		if !ok {
			return
		}

		queries[endpoint].Lock()
		defer queries[endpoint].Unlock()

		for _, id := range ids {
			log.Printf("Deleting %q from endpoint %q", id, endpoint)
			v, ok := queries[endpoint].Available[id]
			if !ok {
				continue
			}
			queries[endpoint].Visited[id] = v
			delete(queries[endpoint].Available, id)
		}
	})
	http.HandleFunc("/listing", func(w http.ResponseWriter, r *http.Request) {
		endpoint := r.URL.Query().Get("endpoint")
		if endpoint == "" {
			return
		}

		queries[endpoint].RLock()
		q := Query{
			Endpoint:  endpoint,
			Available: map[string]result{},
		}
		i := 0
		for k, v := range queries[endpoint].Available {
			q.Available[k] = v
			i += 1
			if i > 100 {
				break
			}
		}
		queries[endpoint].RUnlock()
		if err := tmpl.Execute(w, q); err != nil {
			log.Fatalf("failed to execute template: %v", err)
		}
	})

	log.Fatal(http.ListenAndServe(":8080", nil))
}

var tmpl = template.Must(template.New("rslt").Parse(`
<html>
	<head>
		<title>Busquedas ML</title>
		<script src="http://code.jquery.com/jquery-2.2.1.min.js"></script>
	</head>
	<body>
		<h1>{{.Endpoint}}</h1>
		{{ $endpoint := .Endpoint }}
		<div id='results'>
			{{range $id, $article := .Available}}
			<div class='result'>
				<p>
					<a href="{{$article.Permalink}}">{{$article.Title}} ({{$article.Price}})</a></p>
					<p><img height="250px" width="250px" src="{{$article.Thumbnail}}"/>
					<a class="done" id="{{$article.Id}}" href="" endpoint="{{$endpoint}}">Done</a>
				</p>
			</div>
			{{end}}
		</div>
		<a href="/visitedAll?{{range $id, $article := .Available}}id={{$id}}&{{end}}&endpoint={{$endpoint}}">All Visited</a>
	</body>
	<script>
		$('.done').click(function(e) {
			e.preventDefault();
			var articleId = $(this).attr('id');
			var endpoint = $(this).attr('endpoint');
			$.get("/visited?id=" + articleId + "&endpoint=" + endpoint, function(data) {
				$('#' + data).closest('.result').remove();
			});
		});
	</script>
</html>
`))

