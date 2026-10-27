// auth is the first service of the workspace-rust example. It reads the port
// Phelix injects in PORT and answers two routes over the standard library:
// /health for the deploy health check and /token for its dependents.
use std::env;
use std::io::{BufRead, BufReader, Write};
use std::net::TcpListener;
use std::process;
use std::time::{SystemTime, UNIX_EPOCH};

fn main() {
    let port = env::var("PORT").unwrap_or_else(|_| "8181".to_string());
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
    println!("auth listening on {addr}");

    for stream in listener.incoming() {
        let mut stream = match stream {
            Ok(s) => s,
            Err(_) => continue,
        };

        let path = request_path(&stream);
        let body = route(&path);
        let _ = write_response(&mut stream, &body);
    }
}

// request_path reads only the request line, because the example does not need
// headers or a body. The reader borrows the stream, so it is scoped before the
// response is written back.
fn request_path(stream: &std::net::TcpStream) -> String {
    let mut reader = BufReader::new(stream);
    let mut request_line = String::new();
    if reader.read_line(&mut request_line).is_err() {
        return "/".to_string();
    }
    request_line
        .split_whitespace()
        .nth(1)
        .unwrap_or("/")
        .to_string()
}

// route returns the JSON body for one path. The health route deliberately
// does no work, so a deploy health probe can never fail on a slow clock call.
fn route(path: &str) -> String {
    if path == "/health" {
        return r#"{"service":"auth","status":"ok"}"#.to_string();
    }
    if path == "/token" {
        let issued = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_nanos())
            .unwrap_or(0);
        return format!(r#"{{"token":"demo-{issued}","issued_for":"caller"}}"#);
    }
    r#"{"error":"not found"}"#.to_string()
}

// write_response sends a fixed-length 200 response and closes the connection.
// Only one request per connection is served, which keeps the example small.
fn write_response(stream: &mut std::net::TcpStream, body: &str) -> std::io::Result<()> {
    let response = format!(
        "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
        body.len(),
        body
    );
    stream.write_all(response.as_bytes())
}
