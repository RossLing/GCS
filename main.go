package main
import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"strconv"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/storage"
	"google.golang.org/api/option"

	"github.com/olivere/elastic"
	"github.com/pborman/uuid"
	"github.com/gorilla/mux"
	"github.com/auth0/go-jwt-middleware"
    "github.com/dgrijalva/jwt-go"

)	

const (
	POST_INDEX = "post"
	POST_TYPE = "post"
	DISTANCE = "200km"
    // Needs to update this URL if you deploy it to cloud.
    ES_URL = "http://35.224.186.53:9200"
	BUCKET_NAME = "post-images-0011"
	PROJECT_ID = "around3-227604"
    BT_INSTANCE = "around-post"

)

type Location struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type Post struct {
	 // `json:"user"` is for the json parsing of this User field. Otherwise, by default it's 'User'.
	 User     string `json:"user"`
	 Message  string  `json:"message"`
	 Location Location `json:"location"`
	 Url string `json:"url"`
}

func main() {
	fmt.Println("started-service")
	createIndexIfNotExist()
	
	jwtMiddleware := jwtmiddleware.New(jwtmiddleware.Options{
        ValidationKeyGetter: func(token *jwt.Token) (interface{}, error) {
     	 return []byte(mySigningKey), nil
        },
        SigningMethod: jwt.SigningMethodHS256,
	})
	  
    //http.HandleFunc("/post", handlerPost)
    //http.HandleFunc("/search", handlerSearch)
	r := mux.NewRouter()

    //r.Handle("/post", http.HandlerFunc(handlerPost)).Methods("POST")
    //r.Handle("/search", http.HandlerFunc(handlerSearch)).Methods("GET")
	r.Handle("/post", jwtMiddleware.Handler(http.HandlerFunc(handlerPost))).Methods("POST")
    r.Handle("/search", jwtMiddleware.Handler(http.HandlerFunc(handlerSearch))).Methods("GET")
	r.Handle("/signup", http.HandlerFunc(handlerSignup)).Methods("POST")
	r.Handle("/login", http.HandlerFunc(handlerLogin)).Methods("POST")
 
    http.Handle("/", r)

	
	log.Fatal(http.ListenAndServe(":8080", nil))
      
}
func createIndexIfNotExist() {
    client, err := elastic.NewClient(elastic.SetURL(ES_URL), elastic.SetSniff(false))
    if err != nil {
        panic(err)
    }


	exists,err := client.IndexExists(POST_INDEX).Do(context.Background())
	if err != nil {
		panic(err)
	}
	if !exists {
		mapping := `{
			"mappings": {
				"post": {
					"properties": {
						"location": {
							"type": "geo_point"
						}
					}
				}
			}
		}`

		_, err = client.CreateIndex(POST_INDEX).Body(mapping).Do(context.Background()) 
		if err != nil {
			panic(err)
		}
	}

    exists, err = client.IndexExists(USER_INDEX).Do(context.Background())
    if err != nil {
        panic(err)
    }

    if !exists {
        _, err = client.CreateIndex(USER_INDEX).Do(context.Background())
        if err != nil {
            panic(err)
        }
    }

}

func handlerPost(w http.ResponseWriter, r *http.Request) {
    // Parse from body of request to get a json object.
    fmt.Println("Received one post request")

    w.Header().Set("Content-Type", "application/json")
    w.Header().Set("Access-Control-Allow-Origin", "*")
    w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")

	user := r.Context().Value("user")
    claims := user.(*jwt.Token).Claims
    username := claims.(jwt.MapClaims)["username"]

    lat, _ := strconv.ParseFloat(r.FormValue("lat"), 64)
    lon, _ := strconv.ParseFloat(r.FormValue("lon"), 64)

    p := &Post{
		//User:    r.FormValue("user"),
		User:    username.(string),
        Message: r.FormValue("message"),
        Location: Location{
            Lat: lat,
            Lon: lon,
        },
    }

    id := uuid.New()
    file, _, err := r.FormFile("image")
    if err != nil {
        http.Error(w, "Image is not available", http.StatusBadRequest)
        fmt.Printf("Image is not available %v.\n", err)
        return
    }
    attrs, err := saveToGCS(file, BUCKET_NAME, id)
    if err != nil {
        http.Error(w, "Failed to save image to GCS", http.StatusInternalServerError)
        fmt.Printf("Failed to save image to GCS %v.\n", err)
        return
    }
    p.Url = attrs.MediaLink

    err = saveToES(p, id)
    if err != nil {
        http.Error(w, "Failed to save post to ElasticSearch", http.StatusInternalServerError)
        fmt.Printf("Failed to save post to ElasticSearch %v.\n", err)
        return
	}
	fmt.Printf("Saved one post to ElasticSearch: %s", p.Message)
    saveToBigTable(p, id)
}
// Save a post to BigTable
func saveToBigTable(p *Post, id string) {
	ctx := context.Background()
    bt_client, err := bigtable.NewClient(ctx, "around3-227604", "around-post", option.WithCredentialsFile("key.json"))
	if err != nil {
		panic(err)
		return
	}
	tbl := bt_client.Open("post")
	mut := bigtable.NewMutation()
	t := bigtable.Now()
	mut.Set("post", "user", t, []byte(p.User))
	mut.Set("post", "message", t, []byte(p.Message))
	mut.Set("location", "lat", t, []byte(strconv.FormatFloat(p.Location.Lat, 'f', -1, 64)))
	mut.Set("location", "lon", t, []byte(strconv.FormatFloat(p.Location.Lon, 'f', -1, 64)))

	err = tbl.Apply(ctx, id, mut)
	if err != nil {
		panic(err)
		return
	}
	fmt.Printf("Post is saved to BigTable: %s\n", p.Message)

}
func handlerSearch(w http.ResponseWriter, r *http.Request) {
    fmt.Println("Received one request for search")

    w.Header().Set("Content-Type", "application/json")
    w.Header().Set("Access-Control-Allow-Origin", "*")
    w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")

    lat, _ := strconv.ParseFloat(r.URL.Query().Get("lat"), 64)
    lon, _ := strconv.ParseFloat(r.URL.Query().Get("lon"), 64)
    // range is optional
    ran := DISTANCE
    if val := r.URL.Query().Get("range"); val != "" {
        ran = val + "km"
    }

    posts, err := readFromES(lat, lon, ran)
    if err != nil {
        http.Error(w, "Failed to read post from ElasticSearch", http.StatusInternalServerError)
        fmt.Printf("Failed to read post from ElasticSearch %v.\n", err)
        return
    }

    js, err := json.Marshal(posts)
    if err != nil {
        http.Error(w, "Failed to parse posts into JSON format", http.StatusInternalServerError)
        fmt.Printf("Failed to parse posts into JSON format %v.\n", err)
        return
    }

    w.Write(js)
}
// Save a post to ElasticSearch
func saveToES(post *Post, id string) error {
	client, err := elastic.NewClient(elastic.SetURL(ES_URL),elastic.SetSniff(false))
	if err != nil {
		return err
	}

	_, err = client.Index().
		Index(POST_INDEX).
		Type(POST_TYPE).
		Id(id).
		BodyJson(post).
		Refresh("wait_for").
		Do(context.Background())
	if err != nil {
		// Handle error
		return err
	}

    fmt.Printf("Post is saved to index: %s\n", post.Message)
    return nil
}

func readFromES(lat, lon float64, ran string) ([]Post, error) {
	client, err := elastic.NewClient(elastic.SetURL(ES_URL),elastic.SetSniff(false))
	if err != nil {
		return nil,err
	}
	query := elastic.NewGeoDistanceQuery("location")
	query = query.Distance(ran).Lat(lat).Lon(lon)

	searchResult, err := client.Search().
		Index(POST_INDEX).            // search in index "tweets"
		Query(query).           // specify the query
		Pretty(true).               // pretty print request and response JSON
		Do(context.Background())    // execute
	if err != nil {
		return nil,err
	}

	// searchResult is of type SearchResult and returns hits, suggestions,
	// and all kinds of other information from Elasticsearch.
    fmt.Printf("Query took %d milliseconds\n", searchResult.TookInMillis)

	// Each is a convenience function that iterates over hits in a search result.
	// It makes sure you don't need to check for nil values in the response.
	// However, it ignores errors in serialization. If you want full control
	// over iterating the hits, see below.
	var ptyp Post 
	var posts []Post
	for _, item := range searchResult.Each(reflect.TypeOf(ptyp)) {
		if p, ok := item.(Post); ok {
			posts = append(posts,p)
		}
	}
	//如果（相等）成功就传进来post
    return posts, nil
}

func saveToGCS(r io.Reader, bucketName, objectName string) (*storage.ObjectAttrs, error) {
	ctx := context.Background()
	// Creates a client.
	client, err := storage.NewClient(ctx, option.WithCredentialsFile("key.json"))
	if err != nil {
		return nil,err
	}
	bucket := client.Bucket(bucketName)
	if _, err := bucket.Attrs(ctx); err != nil {
		return nil,err
	}
	object := bucket.Object(objectName)
	wc := object.NewWriter(ctx)
	if _, err = io.Copy(wc,r); err != nil {
		return nil,err
	}
	if err := wc.Close(); err != nil {
		return nil, err
	}

	if err = object.ACL().Set(ctx, storage.AllUsers, storage.RoleReader); err != nil {
        return nil, err
    }

	attrs, err := object.Attrs(ctx)
	if err != nil {
		return nil,err
	}
	fmt.Printf("Image is saved to GCS:%s/n",attrs.MediaLink)
	return attrs,nil
}



