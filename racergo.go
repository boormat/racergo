package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/darkhelmet/env"
	"github.com/mzimmerman/sendgrid-go"
)

type Bib int

var startRaceChan chan time.Time
var raceHasStarted bool = false
var raceStart time.Time
var optionalEntryFields []string
var bibbedEntries map[Bib]*Entry // map of Bib #s pointing to bibbed entries only
var allEntries []*Entry          // slice of all Entries, bibbed and unbibbed
var results []*Result
var auditLog []Audit
var raceResultsTemplate *template.Template
var errorTemplate *template.Template
var prizes []*Prize
var mutex sync.Mutex
var serverHandlers chan bool
var emailIndex = -1 // initialize it to an invalid value
var auditClean bool // used to ensure no changes have taken place before modifying data internally through /audit

var config struct {
	webserverHostname string // the url to serve on - default localhost:8080
	sendgriduser      string // the Sendgrid user for e-mail integration
	sendgridpass      string // the Sendgrid password for e-mail integration
	emailField        string // the title of the Email field in the uploaded CSV - default Email
	emailFrom         string // the from address for the e-mail integration
	raceName          string // Name of the race, default Campus Life 5k Orchard Run
}

const SENDGRIDUSER = "API_USER"
const SENDGRIDPASS = "API_PASS"

func init() {
	config.webserverHostname = env.StringDefault("RACERGOHOSTNAME", "localhost:8080")
	config.sendgriduser = env.StringDefault("RACERGOSENDGRIDUSER", SENDGRIDUSER)
	config.sendgridpass = env.StringDefault("RACERGOSENDGRIDPASS", SENDGRIDPASS)
	config.raceName = env.StringDefault("RACERGORACENAME", "Set RACERGORACENAME environment variable to change race name")
	config.emailField = env.StringDefault("RACERGOEMAILFIELD", "Email")
	config.emailFrom = env.StringDefault("RACERGOFROMEMAIL", "racergo@nonexistenthost.com")
	startRaceChan = make(chan time.Time)
	go listenForRacers()
	numHandlers := runtime.NumCPU()
	runtime.GOMAXPROCS(numHandlers)
	if numHandlers >= 2 {
		// want to leave one cpu not handling racer http requests so as to handle the processing of racers quickly
		numHandlers--
	}
	serverHandlers = make(chan bool, numHandlers)
	for x := 0; x < numHandlers; x++ {
		serverHandlers <- true // fill the channel with valid goroutines
	}
	var err error
	raceResultsTemplate, err = template.ParseFiles("raceResults.template")
	if err != nil {
		log.Fatalf("Error parsing template! - %s\n", err)
		return
	}
	errorTemplate, err = template.ParseFiles("error.template")
	if err != nil {
		log.Fatalf("Error parsing template! - %s\n", err)
		return
	}
}

type HumanDuration time.Duration

type Prize struct {
	Title    string
	LowAge   uint
	HighAge  uint
	Gender   string    // M = only males, F = only Females, O = Overall
	Amount   uint      // how many people win this prize?
	WinAgain bool      // if someone has already won another Prize, can they win this again?
	Winners  []*Result `json:"-"`
}

type Entry struct {
	Bib      Bib
	Fname    string
	Lname    string
	Male     bool
	Age      uint
	Optional []string
	Result   *Result
}

type Audit struct {
	Time   HumanDuration
	Bib    Bib
	Remove bool
}

type Result struct {
	Time      HumanDuration
	Place     uint
	Entry     *Entry
	Confirmed bool
}

type ResultSort []*Result

func (rs *ResultSort) Len() int {
	return len(*rs)
}

func (rs *ResultSort) Less(i, j int) bool {
	return (*rs)[i].Time < (*rs)[j].Time
}

func (rs *ResultSort) Swap(i, j int) {
	(*rs)[i], (*rs)[j] = (*rs)[j], (*rs)[i]
}

func (hd HumanDuration) String() string {
	seconds := time.Duration(hd).Seconds()
	seconds -= float64(time.Duration(hd) / time.Minute * 60)
	return fmt.Sprintf("%#02d:%#02d:%05.2f", time.Duration(hd)/time.Hour, time.Duration(hd)/time.Minute%60, seconds)
}

func (hd HumanDuration) Clock() string {
	return fmt.Sprintf("%#02d:%#02d:%02d", time.Duration(hd)/time.Hour, time.Duration(hd)/time.Minute%60, time.Duration(hd)/time.Second%60)
}

func ParseHumanDuration(val string) (HumanDuration, error) {
	var duration HumanDuration
	str := strings.Split(val, ":")
	if len(str) < 3 {
		return duration, fmt.Errorf("%s is not a valid race duration, must have two semicolons", val)
	}
	secs := strings.Split(str[2], ".")
	if len(secs) < 2 {
		return duration, fmt.Errorf("%s does not contain a valid seconds time, must have a decimal place", val)
	}
	hours, err := strconv.Atoi(str[0])
	if err != nil {
		return duration, fmt.Errorf("Error parsing hours - %s - %v", str[0], err)
	}
	minutes, err := strconv.Atoi(str[1])
	if err != nil {
		return duration, fmt.Errorf("Error parsing minutes - %s - %v", str[1], err)
	}
	seconds, err := strconv.Atoi(secs[0])
	if err != nil {
		return duration, fmt.Errorf("Error parsing seconds - %s - %v", secs[0], err)
	}
	hundredths, err := strconv.Atoi(secs[1])
	if err != nil {
		return duration, fmt.Errorf("Error parsing hundredths - %s - %v", secs[1], err)
	}
	duration = HumanDuration((time.Hour * time.Duration(hours)) + (time.Minute * time.Duration(minutes)) + (time.Second * time.Duration(seconds)) + (time.Millisecond * 10 * time.Duration(hundredths)))
	return duration, nil
}

func download(w http.ResponseWriter, r *http.Request) {
	filename := fmt.Sprintf(config.webserverHostname+"-%s.csv", time.Now().In(time.Local).Format("2006-01-02"))
	w.Header().Set("Content-type", "application/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
	mutex.Lock()
	length := len(allEntries)
	if length > len(results) {
		length = len(results)
	}
	csvData := make([][]string, 0, length+1)
	headerRow := append([]string{"Fname", "Lname", "Age", "Gender", "Bib", "Overall Place", "Time"}, optionalEntryFields...)
	csvData = append(csvData, headerRow)
	for _, entry := range allEntries {
		if entry.Result != nil {
			csvData = append(csvData, append([]string{entry.Fname, entry.Lname, strconv.Itoa(int(entry.Age)), gender(entry.Male), "", "", ""}, entry.Optional...))
		} else {
			csvData = append(csvData, append([]string{entry.Fname, entry.Lname, strconv.Itoa(int(entry.Age)), gender(entry.Male), strconv.Itoa(int(entry.Bib)), "", ""}, entry.Optional...))
		}
	}
	mutex.Unlock()
	writer := csv.NewWriter(w)
	writer.WriteAll(csvData)
	writer.Flush()
}

func gender(male bool) string {
	if male {
		return "M"
	}
	return "F"
}

func uploadPrizes(w http.ResponseWriter, r *http.Request) {
	reader, err := r.MultipartReader()
	if err != nil {
		showErrorForAdmin(w, r.Referer(), "Error getting Reader - %s", err)
		return
	}
	part, err := reader.NextPart()
	if err != nil {
		showErrorForAdmin(w, r.Referer(), "Error getting Part - %s", err)
		return
	}
	jsonin := json.NewDecoder(part)
	mutex.Lock()
	defer mutex.Unlock()
	auditClean = false
	prizes = make([]*Prize, 0)
	for {
		var prize Prize
		err = jsonin.Decode(&prize)
		if err == io.EOF {
			break // good, we processed them all!
		}
		if err != nil {
			showErrorForAdmin(w, r.Referer(), "Error fetching Prize Configurations - %s", err)
			return
		}
		prizes = append(prizes, &prize)
	}
	for _, result := range results {
		if result.Entry == nil {
			break // all done
		}
		calculatePrizes(result)
	}
	http.Redirect(w, r, "/admin", 301)
}

func calculatePrizes(r *Result) {
	// prizes are calculated from top-down, meaning all "faster" racers have already been placed
	if r.Entry == nil {
		return // can't calculate prizes for someone who hasn't finished the race!
	}
	found := false
	// mutex should already be locked in the parent caller
	for _, prize := range prizes {
		switch {
		case found && !prize.WinAgain:
			fallthrough
		case r.Entry.Age < prize.LowAge:
			fallthrough
		case r.Entry.Age > prize.HighAge:
			fallthrough
		case r.Entry.Male && (prize.Gender == "F"):
			fallthrough
		case !r.Entry.Male && (prize.Gender == "M"):
			fallthrough
		case len(prize.Winners) == int(prize.Amount):
			continue // do not qualify any of these conditions
		}
		found = true
		prize.Winners = append(prize.Winners, r)
	}
}

func uploadRacers(w http.ResponseWriter, r *http.Request) {
	reader, err := r.MultipartReader()
	if err != nil {
		showErrorForAdmin(w, r.Referer(), "Error getting Reader - %s", err)
		return
	}
	part, err := reader.NextPart()
	if err != nil {
		showErrorForAdmin(w, r.Referer(), "Error getting Part - %s", err)
		return
	}
	csvIn := csv.NewReader(part)
	rawEntries, err := csvIn.ReadAll()
	if err != nil {
		showErrorForAdmin(w, r.Referer(), "Error Reading CSV file - %s", err)
		return
	}
	if len(rawEntries) <= 1 {
		showErrorForAdmin(w, r.Referer(), "Either blank file or only supplied the header row")
		return
	}

		// make the new in-memory data stores and unlink all previous relationships
	newBibbedEntries := make(map[Bib]*Entry)
	newAllEntries := make([]*Entry, 0, 1024)
	for _, prize := range prizes {
		prize.Winners = make([]*Result, 0)
	}
	// initialize the optionalEntryFields for use when we export/display the data
	newOptionalEntryFields := make([]string, 0)
	mandatoryFields := map[string]struct{}{
		"Fname":  struct{}{},
		"Lname":  struct{}{},
		"Age":    struct{}{},
		"Gender": struct{}{},
	}
	for col := range rawEntries[0] {
		switch rawEntries[0][col] {
		case "Fname":
			fallthrough
		case "Lname":
			fallthrough
		case "Age":
			fallthrough
		case "Gender":
			delete(mandatoryFields, rawEntries[0][col])
		case "Bib": // Bib is a special case but is not mandatory
		default:
			newOptionalEntryFields = append(newOptionalEntryFields, rawEntries[0][col])
		}
	}
	if len(mandatoryFields) > 0 {
		showErrorForAdmin(w, r.Referer(), "CSV file missing the following fields - %s", mandatoryFields)
		return
	}
	// load the data
	for row := 1; row < len(rawEntries); row++ {
		entry := &Entry{Bib: -1}
		entry.Optional = make([]string, 0)
		for col := range rawEntries[row] {
			switch rawEntries[0][col] {
			case "Fname":
				entry.Fname = rawEntries[row][col]
			case "Lname":
				entry.Lname = rawEntries[row][col]
			case "Age":
				tmpAge, _ := strconv.Atoi(rawEntries[row][col])
				entry.Age = uint(tmpAge)
			case "Gender":
				entry.Male = (rawEntries[row][col] == "M")
			case "Bib":
				tmpBib, err := strconv.Atoi(rawEntries[row][col])
				if err != nil {
					entry.Bib = -1
				} else {
					entry.Bib = Bib(tmpBib)
				}
			default:
				entry.Optional = append(entry.Optional, rawEntries[row][col])
			}
		}
		if _, ok := newBibbedEntries[entry.Bib]; ok {
			showErrorForAdmin(w,r.Referer(),"Duplicate bib #%d detected in uploaded CSV file.  Import failed.",entry.Bib)
			return
		}
		if entry.Bib >= 0 {
			newBibbedEntries[entry.Bib] = entry
		}
		newAllEntries = append(newAllEntries, entry)
	}

	mutex.Lock()
	defer mutex.Unlock()
	auditClean = false
	bibbedEntries = newBibbedEntries
	allEntries = newAllEntries
	optionalEntryFields = newOptionalEntryFields
	results = make([]*Result,0,1024)

	emailIndex = -1
	if config.sendgriduser == SENDGRIDUSER || config.sendgridpass == SENDGRIDPASS {
		log.Printf("Sendgrid user/password information not found, not sending result emails")
	} else if config.emailFrom == "" {
		log.Printf("Address to send email from not populated, not sending result emails")
	} else {
		for o, val := range optionalEntryFields {
			if val == config.emailField {
				emailIndex = o
				break
			}
		}
	}
	if emailIndex == -1 {
		log.Printf("No e-mail column of %s found in optionally uploaded fields, not sending result e-mails",config.emailField)
	}
	http.Redirect(w, r, "/admin", 301)
}

func auditPost(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	defer mutex.Unlock()
	if !auditClean {
		showErrorForAdmin(w, r.Referer(), "Data modified since audit record pulled, no updates made.  Try again.")
	}
	auditClean = false
	// wipe the in-memory data stores
	newBibbedEntries := make(map[Bib]*Entry)
	newAllEntries := make([]*Entry, 0, 1024)
	newResults := make([]*Result, 0, 1024)
	for _, prize := range prizes {
		prize.Winners = make([]*Result, 0)
	}
	r.ParseForm()
	// load the new entries
	for row := 0; ; row++ {
		rowString := strconv.Itoa(row) + "."
		entry := &Entry{Bib: -1}
		entry.Optional = make([]string, 0)
		entry.Fname = r.FormValue(rowString + "Fname")
		entry.Lname = r.FormValue(rowString + "Lname")
		tmpAge, _ := strconv.Atoi(r.FormValue(rowString + "Age"))
		entry.Age = uint(tmpAge)
		entry.Male = (r.FormValue(rowString+"Male") == "M")
		tmpBib, err := strconv.Atoi(r.FormValue(rowString + "Bib"))
		if err != nil {
			entry.Bib = -1
		} else {
			entry.Bib = Bib(tmpBib)
		}
		if entry.Fname == "" && entry.Lname == "" && entry.Age == 0 && entry.Bib == -1 {
			break // this one has all default/empty values, must be the end of the records found
		}
		duration, err := ParseHumanDuration(r.FormValue(rowString + "Time"))
		if err != nil {
			fmt.Printf("Unable to parse duration - %v\n", err)
		} else {
			entry.Result = &Result{
				Time:      duration,
				Confirmed: true,
				Entry:     entry,
			}
			newResults = append(newResults, entry.Result)
		}
		for _, opt := range optionalEntryFields {
			entry.Optional = append(entry.Optional, r.FormValue(rowString+opt))
		}
		if entry.Bib >= 0 {
			if _, ok := newBibbedEntries[entry.Bib]; ok {
				showErrorForAdmin(w, r.Referer(), fmt.Sprintf("Cannot assign bib #%d to multiple runners.", entry.Bib))
				return
			}
			newBibbedEntries[entry.Bib] = entry
		}
		newAllEntries = append(newAllEntries, entry)
	}
	// no issues/errors, load the data
	bibbedEntries = newBibbedEntries
	allEntries = newAllEntries
	// now rebuild results
	sort.Sort((*ResultSort)(&newResults))
	for x := range newResults {
		newResults[x].Place = uint(x + 1)
	}
	results = newResults
	recomputeAllPrizes()
	http.Redirect(w, r, "/audit", 301)
}

func startHandler(w http.ResponseWriter, r *http.Request) {
	raceStart = time.Now()
	raceHasStarted = true
	startRaceChan <- raceStart
	http.Redirect(w, r, "/admin", 301)
}

func linkBib(w http.ResponseWriter, r *http.Request) {
	if !raceHasStarted {
		showErrorForAdmin(w, r.Referer(), "Cannot link a bib, the race hasn't started!")
		return
	}
	removeBib := r.FormValue("remove") == "true"
	tmpBib, err := strconv.Atoi(r.FormValue("bib"))
	if err != nil {
		showErrorForAdmin(w, r.Referer(), "Error %s getting bib number", err)
		return
	}
	if tmpBib < 0 {
		showErrorForAdmin(w, r.Referer(), "Cannot assign a negative bib number of %d", tmpBib)
		return
	}
	bib := Bib(tmpBib)
	deltaT := HumanDuration(time.Since(raceStart))
	mutex.Lock()
	defer mutex.Unlock()
	auditClean = false
	auditLog = append(auditLog, Audit{Time: deltaT, Bib: bib, Remove: removeBib})
	entry, ok := bibbedEntries[bib]
	if !ok {
		showErrorForAdmin(w, r.Referer(), "Bib number %d was not assigned to anyone.", bib)
		return
	}
	if removeBib {
		if entry.Result == nil {
			// entry already removed, act successful
			http.Redirect(w, r, "/admin", 301)
			return
		}
		index := int(entry.Result.Place) - 1
		log.Printf("Bib = %d, index = %d, len(results) = %d", bib, index, len(results))
		entry.Result = nil
		if index >= len(results) {
			// something's out of whack here -- The Entry has a Result but the Result isn't in the results slice
			// the fix is removing the entry's result which happens before this if statement
			showErrorForAdmin(w, r.Referer(), "Bib has a result recorded but is not in the results table! - attempted to fix it")
			return
		}
		results = append(results[:index], results[index+1:]...)
		for x := index; x < len(results); x++ {
			results[x].Place = results[x].Place - 1
		}
		http.Redirect(w, r, "/admin", 301)
		return
	}
	if entry.Result != nil {
		if entry.Result.Confirmed {
			showErrorForAdmin(w, r.Referer(), "Bib number %d already confirmed for place #%d", bib, entry.Result.Place)
			return
		}
		entry.Result.Confirmed = true
		http.Redirect(w, r, "/admin", 301)
		if emailIndex == -1 { // no e-mail address was found on data load, just return
			return
		}
		emailAddr := entry.Optional[emailIndex]
		_, err = mail.ParseAddress(emailAddr)
		if err != nil {
			log.Printf("Error parsing e-mail address of %s\n", emailAddr)
			return
		}
		go func(fname, lname, email string, hd HumanDuration) {
			m := sendgrid.NewMail()
			client := sendgrid.NewSendGridClient(config.sendgriduser, config.sendgridpass)
			m.AddTo(fmt.Sprintf("%s %s <%s>", fname, lname, email))
			m.SetSubject(fmt.Sprintf("%s Results", config.raceName))
			m.SetText(fmt.Sprintf("Congratulations %s %s!  You finished the %s in %s!", fname, lname, config.raceName, hd))
			m.SetFrom(config.emailFrom)
			backoff := time.Second
			for {
				err := client.Send(m)
				if err == nil {
					log.Printf("Success sending %#v", m)
					return
				}
				backoff = backoff * 2
				log.Printf("Error sending mail to %s - %v, trying again in %s", email, err, backoff)
				time.Sleep(backoff)
			}
		}(entry.Fname, entry.Lname, emailAddr, entry.Result.Time)
		return
	}
	result := &Result{
		Time:      deltaT,
		Place:     uint(len(results) + 1),
		Confirmed: false,
		Entry:     entry,
	}
	results = append(results, result)
	entry.Result = result
	log.Printf("Set bib for place %d to %d\n", result.Place, bib)
	calculatePrizes(result)
	http.Redirect(w, r, "/admin", 301)
}

func showErrorForAdmin(w http.ResponseWriter, referrer string, message string, args ...interface{}) {
	w.WriteHeader(409) // conflict header, most likely due to old information in the client
	msg := fmt.Sprintf(message, args...)
	log.Println(msg)
	if errorTemplate == nil {
		fmt.Fprintf(w, msg)
		return
	}
	err := errorTemplate.Execute(w, map[string]interface{}{"Message": msg, "Referrer": referrer})
	if err != nil {
		fmt.Fprintf(w, "Error executing template - %s", err)
	}
}

// mutex needs to be locked already when calling this
func recomputeAllPrizes() {
	// now need to recompute the prize results
	for _, prize := range prizes {
		prize.Winners = make([]*Result, 0)
	}
	for _, result := range results {
		if result.Entry == nil {
			break // all done
		}
		calculatePrizes(result)
	}
}

func assignBib(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.FormValue("id"))
	if err != nil {
		showErrorForAdmin(w, r.Referer(), r.Referer(), "Error %s getting next", err)
		return
	}
	tmpBib, err := strconv.Atoi(r.FormValue("bib"))
	if tmpBib < 0 || err != nil {
		showErrorForAdmin(w, r.Referer(), "Could not get a valid bib number from %s", r.FormValue("bib"))
		return
	}
	bib := Bib(tmpBib)
	mutex.Lock()
	defer mutex.Unlock()

	if len(allEntries) > id {
		entry := allEntries[id]
		if _, ok := bibbedEntries[bib]; ok {
			showErrorForAdmin(w, r.Referer(), "Bib # %d already assigned to %s %s!", bib, bibbedEntries[bib].Fname, bibbedEntries[bib].Lname)
			return
		}
		entry.Bib = bib
		log.Printf("Set bib for %s %s to %d", entry.Fname, entry.Lname, bib)
		bibbedEntries[entry.Bib] = entry
	} else {
		showErrorForAdmin(w, r.Referer(), "Id %d was not assigned to anyone.", id)
		return
	}
	http.Redirect(w, r, "/admin", 301)
	return
}

func addEntry(w http.ResponseWriter, r *http.Request) {
	entry := &Entry{}
	age, err := strconv.Atoi(r.FormValue("Age"))
	if age < 0 {
		showErrorForAdmin(w, r.Referer(), "Not a valid age, must be >= 0")
		return
	}
	if err != nil {
		showErrorForAdmin(w, r.Referer(), "Error %s getting Age", err)
		return
	}
	entry.Age = uint(age)
	tmpBib, err := strconv.Atoi(r.FormValue("Bib"))
	entry.Bib = Bib(tmpBib)
	if entry.Bib < 0 {
		showErrorForAdmin(w, r.Referer(), "Not a valid bib, must be >= 0")
		return
	}
	if err != nil {
		showErrorForAdmin(w, r.Referer(), "Error %s getting Bib", err)
		return
	}
	entry.Fname = r.FormValue("Fname")
	entry.Lname = r.FormValue("Lname")
	entry.Male = r.FormValue("Male") == "true"
	entry.Optional = make([]string, 0)
	mutex.Lock()
	defer mutex.Unlock()
	auditClean = false
	for _, s := range optionalEntryFields {
		entry.Optional = append(entry.Optional, r.FormValue(s))
	}
	if bibbedEntries == nil {
		bibbedEntries = make(map[Bib]*Entry)
	}
	if _, ok := bibbedEntries[entry.Bib]; ok {
		showErrorForAdmin(w, r.Referer(), "Bib #%d already assigned", entry.Bib)
		return
	}
	bibbedEntries[entry.Bib] = entry
	allEntries = append(allEntries, entry)
	log.Printf("Added Entry - %#v\n", entry)
	http.Redirect(w, r, "/admin", 301)
	return
}

func handler(w http.ResponseWriter, r *http.Request) {
	<-serverHandlers // wait until a goroutine to handle http requests is free
	mutex.Lock()
	defer func() {
		mutex.Unlock()
		serverHandlers <- true // wait for handler to finish, then put it back in the queue so another handler can work
	}()
	data := map[string]interface{}{"Racers": results}
	name := strings.Trim(r.URL.Path, "/")
	switch name {
	default:
		name = "default"
	case "admin":
		recentRacers := make([]*Result, 0, len(results))
		end := len(results)
		for {
			end--
			if end < 0 {
				break
			}
			recentRacers = append(recentRacers, results[end])
			if end < len(results)-10 { // list no more than 10 most recent
				break
			}
		}
		data["RecentRacers"] = recentRacers
		fallthrough
	case "audit":
		data["Admin"] = true
		data["Audit"] = auditLog
		auditClean = true
		if len(allEntries) > 0 {
			data["Entries"] = allEntries
		}
		data["Fields"] = optionalEntryFields
	case "results":
		recentRacers := make([]*Result, 0, len(results))
		end := len(results)
		for {
			end--
			if end < 0 {
				break
			}
			recentRacers = append(recentRacers, results[end])
		}
		data["RecentRacers"] = recentRacers
	}
	if raceHasStarted {
		diff := time.Since(raceStart)
		data["Start"] = raceStart.Format("3:04:05")
		data["Time"] = HumanDuration(diff).Clock()
		data["Seconds"] = fmt.Sprintf("%.0f", diff.Seconds())
		data["NextUpdate"] = diff / time.Millisecond % 1000
	}
	data["Prizes"] = prizes
	raceResultsTemplate, _ = template.ParseFiles("raceResults.template")
	err := raceResultsTemplate.ExecuteTemplate(w, name, data)
	if err != nil {
		fmt.Fprintf(w, "Error executing template - %s", err)
	}
}

func uploadFile(filename string) (*http.Request, error) {
	// Create buffer
	buf := new(bytes.Buffer) // caveat IMO dont use this for large files, \
	// create a tmpfile and assemble your multipart from there (not tested)
	w := multipart.NewWriter(buf)
	// Create a form field writer for field label
	fw, err := w.CreateFormFile("upload", filename)
	if err != nil {
		return nil, err
	}
	fd, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer fd.Close()
	// Write file field from file to upload
	_, err = io.Copy(fw, fd)
	if err != nil {
		return nil, err
	}
	// Important if you do not close the multipart writer you will not have a
	// terminating boundry
	w.Close()
	req, err := http.NewRequest("POST", "", buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req, nil
	//io.Copy(os.Stderr, res.Body) // Replace this with Status.Code check
}

func reset() {
	log.Printf("Initializing the race")
	raceHasStarted = false
	results = make([]*Result, 0, 1024)
	auditLog = make([]Audit, 0, 1024)
	req, err := uploadFile("prizes.json")
	if err == nil {
		resp := httptest.NewRecorder()
		uploadPrizes(resp, req)
		if resp.Code != 301 {
			log.Println("Unable to load the default prizes.json file.")
		}
	} else {
		log.Printf("Unable to load the default prizes.json file - %v\n", err)
	}
}

func main() {
	reset()
	http.HandleFunc(config.webserverHostname+"/", handler)
	http.HandleFunc(config.webserverHostname+"/admin", handler)
	http.HandleFunc(config.webserverHostname+"/start", startHandler)
	http.HandleFunc(config.webserverHostname+"/linkBib", linkBib)
	http.HandleFunc(config.webserverHostname+"/assignBib", assignBib)
	http.HandleFunc(config.webserverHostname+"/addEntry", addEntry)
	http.HandleFunc(config.webserverHostname+"/download", download)
	http.HandleFunc(config.webserverHostname+"/uploadRacers", uploadRacers)
	http.HandleFunc(config.webserverHostname+"/uploadPrizes", uploadPrizes)
	http.HandleFunc(config.webserverHostname+"/auditPost", auditPost)
	http.Handle(config.webserverHostname+"/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static/"))))
	http.Handle(config.webserverHostname+"/fonts/", http.StripPrefix("/fonts/", http.FileServer(http.Dir("fonts/"))))
	http.Handle("/", http.RedirectHandler("http://"+config.webserverHostname+"/", 307))
	log.Printf("Starting http server")
	listener, err := net.Listen("tcp", ":80")
	if err != nil {
		log.Printf("Error listening on port 80, trying 8080 instead! - %s\n", err)
		listener, err = net.Listen("tcp4", ":8080")
		if err != nil {
			log.Fatalf("Error listening on port 8080! - %s\n", err)
			return
		}
	}
	port := strings.Split(listener.Addr().String(), ":")
	portNum := port[len(port)-1]
	log.Printf("Server listening on port %s\n", portNum)
	log.Printf("Basic - http://localhost:%s", portNum)
	log.Printf("Admin - http://localhost:%s/admin", portNum)
	log.Printf("Audit - http://localhost:%s/audit", portNum)
	log.Printf("Large Screen Live Results - http://localhost:%s/results", portNum)
	err = http.Serve(listener, nil)
	if err != nil {
		log.Fatalf("Error starting http server! - %s\n", err)
	}
}

func listenForRacers() {
	ticker := time.NewTicker(time.Second * 10)
	var start time.Time
	for {
		select {
		case start = <-startRaceChan:
			ticker.Stop() // stop and "upgrade" the ticker for every second to track time
			ticker = time.NewTicker(time.Second)
			log.Printf("Race started @ %s\n", start.Format("3:04:05"))
		case now := <-ticker.C:
			if raceHasStarted {
				log.Println(HumanDuration(now.Sub(start)))
			} else {
				log.Println("Waiting to start the race")
			}
			// update the clock
		}
	}
}
