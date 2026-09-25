// simple-rust: the smallest Rust application Phelix can build, run, and deploy.
// It reads the PORT that Phelix injects and serves plain HTTP with the standard
// library only — no external crates.
use std::env;
use std::io::{BufRead, BufReader, Write};
use std::net::TcpListener;
use std::process;

fn main() {
    // Phelix injects PORT for each managed instance. Fall back to 8080 only so
    // the example is also runnable directly with `cargo run`.
    let port = env::var("PORT").unwrap_or_else(|_| "8080".to_string());

    // u16 parsing rejects non-numbers and anything above 65535; reject 0 too.
    let port_num: u16 = match port.parse() {
        Ok(n) if n >= 1 => n,
        _ => {
            eprintln!("invalid PORT {port:?}: must be an integer in 1-65535");
            process::exit(1);
        }
    };

    let addr = format!("0.0.0.0:{port_num}");
    let listener = match TcpListener::bind(&addr) {
        Ok(l) => l,
        Err(e) => {
            eprintln!("failed to bind {addr}: {e}");
            process::exit(1);
        }
    };
    println!("simple-rust listening on {addr}");

    for stream in listener.incoming() {
        let mut stream = match stream {
            Ok(s) => s,
            Err(_) => continue,
        };

        // Read only the request line so we can route on the path. The reader
        // borrows the stream, so scope it before we write the response back.
        let path = {
            let mut reader = BufReader::new(&stream);
            let mut request_line = String::new();
            if reader.read_line(&mut request_line).is_err() {
                continue;
            }
            request_line
                .split_whitespace()
                .nth(1)
                .unwrap_or("/")
                .to_string()
        };

        let body = if path == "/health" {
            "ok\n".to_string()
        } else {
            format!("hello from simple-rust on port {port_num}\n")
        };
        let response = format!(
            "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
            body.len(),
            body
        );
        let _ = stream.write_all(response.as_bytes());
    }
}
