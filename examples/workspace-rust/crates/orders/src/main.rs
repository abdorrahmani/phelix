// orders is the middle service of the workspace-rust example. It calls auth
// when it builds the order list, which shows the dependency the workspace
// manifest declares. Standard library only.
use std::env;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::process;
use std::time::Duration;

// AUTH_ADDR is where orders reaches auth. Both services run on one host, so
// localhost is correct; a larger mesh would take this from configuration.
const AUTH_ADDR: &str = "localhost:8181";

fn main() {
    let port = env::var("PORT").unwrap_or_else(|_| "8182".to_string());
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
    println!("orders listening on {addr}");

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

// route returns the JSON body for one path. The health route never calls auth,
// so an auth outage cannot fail a deploy of orders.
fn route(path: &str) -> String {
    if path == "/health" {
        return r#"{"service":"orders","status":"ok"}"#.to_string();
    }
    if path != "/orders" {
        return r#"{"error":"not found"}"#.to_string();
    }
    match fetch_token() {
        Ok(token) => format!(
            r#"{{"orders":[{{"id":1,"customer":"ada","total_cents":4200}},{{"id":2,"customer":"grace","total_cents":1900}}],"auth_token":"{token}"}}"#
        ),
        Err(e) => format!(
            r#"{{"orders":[{{"id":1,"customer":"ada","total_cents":4200}}],"auth_error":"{e}"}}"#
        ),
    }
}

// fetch_token asks auth for a token over a one-shot HTTP request. The response
// is scanned for the token field rather than parsed, which keeps the example
// free of a JSON dependency.
fn fetch_token() -> Result<String, String> {
    let mut stream = TcpStream::connect(AUTH_ADDR).map_err(|e| format!("auth unreachable: {e}"))?;
    let _ = stream.set_read_timeout(Some(Duration::from_secs(2)));

    let request = "GET /token HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n";
    stream
        .write_all(request.as_bytes())
        .map_err(|e| format!("auth write failed: {e}"))?;

    let mut response = String::new();
    stream
        .read_to_string(&mut response)
        .map_err(|e| format!("auth read failed: {e}"))?;

    response
        .split("\"token\":\"")
        .nth(1)
        .and_then(|rest| rest.split('"').next())
        .map(|token| token.to_string())
        .ok_or_else(|| "auth response has no token".to_string())
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
