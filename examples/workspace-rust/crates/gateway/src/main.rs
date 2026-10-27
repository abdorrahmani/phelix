// gateway is the last service of the workspace-rust example. It forwards
// /auth/* to auth and /orders/* to orders, stripping the prefix, which is why
// the manifest lists it as depending on both. Standard library only.
use std::env;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::process;
use std::time::Duration;

// UPSTREAMS maps a gateway prefix to the address that answers it.
const UPSTREAMS: [(&str, &str); 2] = [
    ("/auth/", "localhost:8181"),
    ("/orders/", "localhost:8182"),
];

fn main() {
    let port = env::var("PORT").unwrap_or_else(|_| "8180".to_string());
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
    println!("gateway listening on {addr}");

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
// headers or a body.
fn request_path(stream: &TcpStream) -> String {
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

// route answers /health and / locally and forwards everything else to the
// upstream whose prefix matches. Health never proxies, so a failing upstream
// cannot fail a gateway deploy.
fn route(path: &str) -> String {
    if path == "/health" {
        return r#"{"service":"gateway","status":"ok"}"#.to_string();
    }
    if path == "/" {
        return r#"{"service":"gateway","routes":["/auth/token","/orders/orders","/health"]}"#
            .to_string();
    }
    for (prefix, addr) in UPSTREAMS {
        if let Some(rest) = path.strip_prefix(prefix) {
            return forward(addr, &format!("/{rest}"))
                .unwrap_or_else(|e| format!(r#"{{"error":"upstream {addr}: {e}"}}"#));
        }
    }
    r#"{"error":"not found"}"#.to_string()
}

// forward performs one GET against an upstream and returns its response body.
// The read timeout keeps a stuck upstream from holding the gateway forever.
fn forward(addr: &str, path: &str) -> Result<String, String> {
    let mut stream = TcpStream::connect(addr).map_err(|e| format!("unreachable: {e}"))?;
    let _ = stream.set_read_timeout(Some(Duration::from_secs(10)));

    let request = format!("GET {path} HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n");
    stream
        .write_all(request.as_bytes())
        .map_err(|e| format!("write failed: {e}"))?;

    let mut response = String::new();
    stream
        .read_to_string(&mut response)
        .map_err(|e| format!("read failed: {e}"))?;

    let (_, body) = response
        .split_once("\r\n\r\n")
        .ok_or_else(|| "malformed upstream response".to_string())?;
    Ok(body.to_string())
}

// write_response sends a fixed-length 200 response and closes the connection.
fn write_response(stream: &mut TcpStream, body: &str) -> std::io::Result<()> {
    let response = format!(
        "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
        body.len(),
        body
    );
    stream.write_all(response.as_bytes())
}
