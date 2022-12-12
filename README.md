# testrun

Program testrun is used to repeatedly run a program to help diagnose
intermitten bugs.  It is designed work with the the Go programming language's
```go test``` testing.

Normally testrun is called with the path to the source of the Go package that
is under test.  Only a single package is supported.  Testrun changes
directory to the Go package's directory and builds the test binary from
there.  The built test binary is placed in a temporary directory created by
testrun.

The test binary is built as if the following commands were run:

```
mkdir $RUNDIR
cd $PACKAGEDIR
go test -c -o $RUNDER/test.binary
```

Alternatively, the ```--binary``` option can be used to specify the test binary.
In this case go test is not run.  Testrun makes no requirements of the test
binary other than it is an executable.

Normally testrun deletes the directories it creates.  If the output of the
tests is desired, the ```--dir``` option can be used to specify where testrun
should store results.  Normally the specified directory must not exist.  When
the ```-f``` option is provided, testrun will first attempt to remove the specified
directory prior to checking to see if it already exists.

By default the test is run serially until failure or the test is interrupted.
The ```--duration``` option is used to limit the amount of time the test will run
and ```-n``` is used to specifiy the maximum number of runs.  These may be used
together.  Further, the ```--max``` option can be used to run multiple tests in
parallel.

If ```--continue``` is specified, testrun does not terminate after the first
failure.  It continues to run until the duration limit (```--duration```) is
reached, the maximum number of tests is reached (```-n```), or an interrupt signal
is sent.

By default each tests standard output and standard error are written to
testrun's standard output and standard error.  In addition, when ```--dir``` is
used and the test binary returns with a status code other than 0, the
standard output and standard error, if any, is written to a subdirectoy of
the specified directory.

Use the ```--silent``` option to prevent test output from being written to
testrun's standard output and standard error.  The use of ```--silent``` does not
prevent the output being written to files in testrun's directory (```--dir```).
