## Benchmark various golang things in my application

#go tool pprof -png http://localhost:8000/debug/pprof/profile > benchmarks/profile.png 
go tool pprof -png http://localhost:8000/debug/pprof/trace > benchmarks/trace.png
go tool pprof -png http://localhost:8000/debug/pprof/heap > benchmarks/heap.png
go tool pprof -png http://localhost:8000/debug/pprof/goroutine > benchmarks/goroutine.png
go tool pprof -png http://localhost:8000/debug/pprof/block > benchmarks/block.png
