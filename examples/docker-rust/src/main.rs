// docker-rust-demo: a minimal Rust HTTP service used to demonstrate the Phelix
// Docker runtime. Phelix builds an image for this app and runs each instance as
// a container, injecting PORT exactly as it does for a native process. Standard
// library only — no external crates.
use std::env;
use std::io::{BufRead, BufReader, Write};
use std::net::TcpListener;
use std::process;

fn main() {
    // Phelix injects PORT into the container for each instance; the app binds
    // it. Nothing is EXPOSEd in the Dockerfile — Phelix publishes the container
    // port to a private 127.0.0.1 host port itself.
    let port = env::var("PORT").unwrap_or_else(|_| "8080".to_string());

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
    println!("docker-rust-demo listening on {addr}");

    for stream in listener.incoming() {
        let mut stream = match stream {
            Ok(s) => s,
            Err(_) => continue,
        };

        // Read only the request line to route on the path; scope the reader's
        // borrow of the stream before writing the response back.
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
            format!("hello from docker-rust-demo on port {port_num}\n")
        };
        let response = format!(
            "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
            body.len(),
            body
        );
        let _ = stream.write_all(response.as_bytes());
    }
}
