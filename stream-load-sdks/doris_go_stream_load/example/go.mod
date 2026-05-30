module doris_go_stream_load_example

go 1.21

require github.com/apache/doris-sdk/stream-load-sdks/doris_go_stream_load v1.0.0

require golang.org/x/crypto v0.33.0 // indirect

replace github.com/apache/doris-sdk/stream-load-sdks/doris_go_stream_load => ..
