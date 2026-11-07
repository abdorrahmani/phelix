module example.com/app

go 1.24

require example.com/direct v1.2.3

require (
	example.com/indirect v0.1.0 // indirect
	example.com/pinned v1.0.0
	example.com/local v1.0.0
)

replace example.com/direct => example.com/fork v1.5.0

replace example.com/pinned v1.0.0 => example.com/pinned v1.0.1

replace example.com/local => ../local-lib

exclude example.com/old v0.9.0
