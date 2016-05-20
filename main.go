// HTTP/2 web server with built-in support for Lua, Markdown, GCSS, Amber and JSX.
package main

import (
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	internallog "log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"syscall"
	"time"

	"github.com/xyproto/pinterface"
	"github.com/xyproto/unzip"
	"github.com/yuin/gopher-lua"
)

const (
	versionString = "Algernon 0.91"
	description   = "HTTP/2 Web Server"
)

var (
	// For convenience. Set in the main function.
	serverHost      string
	dbName          string
	refreshDuration time.Duration
	fs              *FileStat
)

func main() {
	var err error

	// Will be default in Go 1.5
	runtime.GOMAXPROCS(runtime.NumCPU())

	// Temporary directory that might be used for logging, databases or file extraction
	serverTempDir, err := ioutil.TempDir("", "algernon")
	if err != nil {
		log.Fatalln(err)
	}
	defer os.RemoveAll(serverTempDir)

	// Set several configuration variables, based on the given flags and arguments
	serverHost = handleFlags(serverTempDir)

	// Version
	if showVersion {
		if !quietMode {
			fmt.Println(versionString)
		}
		os.Exit(0)
	}

	// CPU profiling
	if profileCPU != "" {
		f, err := os.Create(profileCPU)
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			log.Info("Profiling CPU usage")
			pprof.StartCPUProfile(f)
		}()
		atShutdown(func() {
			pprof.StopCPUProfile()
			log.Info("Done profiling CPU usage")
		})
	}

	// Memory profiling at server shutdown
	if profileMem != "" {
		atShutdown(func() {
			f, err := os.Create(profileMem)
			defer f.Close()
			if err != nil {
				log.Fatal(err)
			}
			log.Info("Saving heap profile to ", profileMem)
			pprof.WriteHeapProfile(f)
		})
	}

	// Request handlers
	mux := http.NewServeMux()

	// Read mime data from the system, if available
	initializeMime()

	// Create a new FileStat struct, with optional caching (for speed).
	// Clear the cache every 10 minutes.
	fs = NewFileStat(cacheFileStat, time.Minute*10)

	// Check if the given directory really is a directory
	if !fs.isDir(serverDir) {
		// Possibly a file
		filename := serverDir
		// Check if the file exists
		if fs.exists(filename) {
			// Switch based on the lowercase filename extension
			switch strings.ToLower(filepath.Ext(filename)) {
			case ".md", ".markdown":
				// Serve the given Markdown file as a static HTTP server
				serveStaticFile(filename, defaultWebColonPort)
				return
			case ".zip", ".alg":
				// Assume this to be a compressed Algernon application
				err := unzip.Extract(filename, serverTempDir)
				if err != nil {
					log.Fatalln(err)
				}
				// Use the directory where the file was extracted as the server directory
				serverDir = serverTempDir
				// If there is only one directory there, assume it's the
				// directory of the newly extracted ZIP file.
				if filenames := getFilenames(serverDir); len(filenames) == 1 {
					fullPath := filepath.Join(serverDir, filenames[0])
					if fs.isDir(fullPath) {
						// Use this as the server directory instead
						serverDir = fullPath
					}
				}
				// If there are server configuration files in the extracted
				// directory, register them.
				for _, filename := range serverConfigurationFilenames {
					configFilename := filepath.Join(serverDir, filename)
					if fs.exists(configFilename) {
						serverConfigurationFilenames = append(serverConfigurationFilenames, configFilename)
					}
				}
				// Disregard all configuration files from the current directory
				// (filenames without a path separator), since we are serving a
				// ZIP file.
				for i, filename := range serverConfigurationFilenames {
					if strings.Count(filepath.ToSlash(filename), "/") == 0 {
						// Remove the filename from the slice
						serverConfigurationFilenames = append(serverConfigurationFilenames[:i], serverConfigurationFilenames[i+1:]...)
					}
				}
			default:
				singleFileMode = true
			}
		} else {
			fatalExit(errors.New("File does not exist: " + filename))
		}
	}

	// Make a few changes to the defaults if we are serving a single file
	if singleFileMode {
		debugMode = true
		serveJustHTTP = true
	}

	// Console output
	if !quietMode && !singleFileMode && !simpleMode {
		fmt.Println(banner())
	}

	// Dividing line between the banner and output from any of the configuration scripts
	if len(serverConfigurationFilenames) > 0 && !quietMode {
		fmt.Println("--------------------------------------- - - · ·")
	}

	// Disable the database backend if the BoltDB filename is /dev/null
	if boltFilename == "/dev/null" {
		useNoDatabase = true
	}

	var perm pinterface.IPermissions // nil by default
	if !useNoDatabase {
		// Connect to a database and retrieve a Permissions struct
		perm, err = aquirePermissions()
		if err != nil {
			log.Fatalln("Could not find a usable database backend.")
		}
	}

	// Lua LState pool
	luapool := &lStatePool{saved: make([]*lua.LState, 0, 4)}
	atShutdown(func() {
		luapool.Shutdown()
	})

	// TODO: save repl history + close luapool + close logs ++ at shutdown

	// Create a cache struct for reading files (contains functions that can
	// be used for reading files, also when caching is disabled).
	// The final argument is for compressing with "fast" instead of "best".
	cache := newFileCache(cacheSize, cacheCompression, cacheMaxEntitySize)

	if singleFileMode && filepath.Ext(serverDir) == ".lua" {
		luaServerFilename = serverDir
		if luaServerFilename == "index.lua" || luaServerFilename == "data.lua" {
			log.Warn("Using " + luaServerFilename + " as a standalone server!\nYou might wish to serve a directory instead.")
		}
		serverDir = filepath.Dir(serverDir)
		singleFileMode = false
	}

	// Log to a file as JSON, if a log file has been specified
	if serverLogFile != "" {
		f, err := os.OpenFile(serverLogFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, defaultPermissions)
		if err != nil {
			log.Error("Could not log to", serverLogFile)
			fatalExit(err)
		}
		log.SetFormatter(&log.JSONFormatter{})
		log.SetOutput(f)
	} else if quietMode {
		// If quiet mode is enabled and no log file has been specified, disable logging
		log.SetOutput(ioutil.Discard)
	}

	if quietMode {
		os.Stdout.Close()
		os.Stderr.Close()
	}

	// Read server configuration script, if present.
	// The scripts may change global variables.
	var ranConfigurationFilenames []string
	for _, filename := range serverConfigurationFilenames {
		if fs.exists(filename) {
			if verboseMode {
				fmt.Println("Running configuration file: " + filename)
			}
			if err := runConfiguration(filename, perm, luapool, cache, mux, false); err != nil {
				log.Error("Could not use configuration script: " + filename)
				if perm != nil {
					fatalExit(err)
				} else {
					log.Warn("Ignoring script error since database backend is disabled.")
				}
			}
			ranConfigurationFilenames = append(ranConfigurationFilenames, filename)
		}
	}
	// Only keep the active ones. Used when outputting server information.
	serverConfigurationFilenames = ranConfigurationFilenames

	// Run the standalone Lua server, if specified
	if luaServerFilename != "" {
		// Run the Lua server file and set up handlers
		if verboseMode {
			fmt.Println("Running Lua Server File")
		}
		if err := runConfiguration(luaServerFilename, perm, luapool, cache, mux, true); err != nil {
			log.Error("Error in Lua server script: " + luaServerFilename)
			fatalExit(err)
		}
	} else {
		// Register HTTP handler functions
		registerHandlers(mux, "/", serverDir, perm, luapool, cache, serverAddDomain)
	}

	// Set the values that has not been set by flags nor scripts
	// (and can be set by both)
	ranServerReadyFunction := finalConfiguration(serverHost)

	// If no configuration files were being ran successfully,
	// output basic server information.
	if len(serverConfigurationFilenames) == 0 {
		if !quietMode {
			fmt.Println(serverInfo())
		}
		ranServerReadyFunction = true
	}

	// Dividing line between the banner and output from any of the
	// configuration scripts. Marks the end of the configuration output.
	if ranServerReadyFunction && !quietMode {
		fmt.Println("--------------------------------------- - - · ·")
	}

	// Direct internal logging elsewhere
	internalLogFile, err := os.Open(internalLogFilename)
	defer internalLogFile.Close()

	if err != nil {
		// Could not open the internalLogFilename filename, try using another filename
		internalLogFile, err = os.OpenFile("internal.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, defaultPermissions)
		atShutdown(func() {
			// TODO This one is is special and should be closed after the other shutdown functions.
			//      Set up a "done" channel instead of sleeping.
			time.Sleep(100 * time.Millisecond)
			internalLogFile.Close()
		})
		if err != nil {
			fatalExit(fmt.Errorf("Could not write to %s nor %s.", internalLogFilename, "internal.log"))
		}
	}
	internallog.SetOutput(internalLogFile)

	// Serve filesystem events in the background.
	// Used for reloading pages when the sources change.
	// Can also be used when serving a single file.
	if autoRefreshMode {
		refreshDuration, err = time.ParseDuration(eventRefresh)
		if err != nil {
			log.Warn(fmt.Sprintf("%s is an invalid duration. Using %s instead.", eventRefresh, defaultEventRefresh))
			// Ignore the error, since defaultEventRefresh is a constant and must be parseable
			refreshDuration, _ = time.ParseDuration(defaultEventRefresh)
		}
		if autoRefreshDir != "" {
			// Only watch the autoRefreshDir, recursively
			EventServer(eventAddr, defaultEventPath, autoRefreshDir, refreshDuration, "*")
		} else {
			// Watch everything in the server directory, recursively
			EventServer(eventAddr, defaultEventPath, serverDir, refreshDuration, "*")
		}
	}

	// For communicating to and from the REPL
	ready := make(chan bool) // for when the server is up and running
	done := make(chan bool)  // for when the user wish to quit the server

	// The Lua REPL
	if !serverMode {
		// If the REPL uses readline, the SIGWINCH signal is handled
		go REPL(perm, luapool, cache, ready, done)
	} else {
		// Ignore SIGWINCH if we are not going to use a REPL
		if runtime.GOOS != "windows" {
			signal.Ignore(syscall.SIGWINCH)
		}
	}

	shutdownTimeout := 10 * time.Second

	conf := &algernonServerConfig{
		productionMode:      productionMode,
		serverHost:          serverHost,
		serverAddr:          serverAddr,
		serverCert:          serverCert,
		serverKey:           serverKey,
		serveJustHTTP:       serveJustHTTP,
		serveJustHTTP2:      serveJustHTTP2,
		shutdownTimeout:     shutdownTimeout,
		internalLogFilename: internalLogFilename,
	}

	// Run the shutdown functions if graceful does not
	defer generateShutdownFunction(nil)()

	// Serve HTTP, HTTP/2 and/or HTTPS
	if err := serve(conf, mux, done, ready); err != nil {
		fatalExit(err)
	}
}
