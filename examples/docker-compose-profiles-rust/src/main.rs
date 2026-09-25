// profiles-rust-demo demonstrates PATTERN C in Rust: the app is defined in
// docker-compose for LOCAL dev (under a compose profile), but on the SERVER only
// Phelix runs it. The image is built the default way (a Dockerfile); the app is
// never a compose service on the server, so `docker compose up -d` there starts
// only the backing tier. Standard library only — a raw RESP PING, no crate.
use std::env;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream, ToSocketAddrs};
use std::process;
use std::time::Duration;

fn main() {
    // Phelix injects PORT; the app binds it (same PORT contract as a native app).
    let port = env::var("PORT").unwrap_or_else(|_| "8080".to_string());
    let port_num: u16 = match port.parse() {
        Ok(n) if n >= 1 => n,
        _ => {
            eprintln!("invalid PORT {port:?}: must be an integer in 1-65535");
            process::exit(1);
        }
    };
    // Reached by docker-compose service name over the shared network. Overridable
    // with `phelix env set profiles-rust-demo REDIS_ADDR ...`; the default matches
    // the compose service.
    let redis_addr = env::var("REDIS_ADDR").unwrap_or_else(|_| "redis:6379".to_string());

    let addr = format!("0.0.0.0:{port_num}");
    let listener = match TcpListener::bind(&addr) {
        Ok(l) => l,
        Err(e) => {
            eprintln!("failed to bind {addr}: {e}");
            process::exit(1);
        }
    };
    println!("profiles-rust-demo listening on {addr} (redis at {redis_addr})");

    for stream in listener.incoming() {
        let mut stream = match stream {
            Ok(s) => s,
            Err(_) => continue,
        };
        let path = {
            let mut reader = BufReader::new(&stream);
            let mut line = String::new();
            if reader.read_line(&mut line).is_err() {
                continue;
            }
            line.split_whitespace().nth(1).unwrap_or("/").to_string()
        };

        let (status, body) = match path.as_str() {
            // Liveness only — independent of Redis so a backing blip never fails
            // the blue-green health gate.
            "/health" => ("200 OK", "ok\n".to_string()),
            "/redis" => match redis_ping(&redis_addr) {
                Ok(pong) => ("200 OK", format!("redis {redis_addr} -> {pong}\n")),
                Err(e) => (
                    "503 Service Unavailable",
                    format!("redis {redis_addr} unreachable: {e}\n"),
                ),
            },
            _ => (
                "200 OK",
                format!("hello from profiles-rust-demo on port {port_num}\n"),
            ),
        };
        let response = format!(
            "HTTP/1.1 {status}\r\nContent-Type: text/plain\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
            body.len(),
            body
        );
        let _ = stream.write_all(response.as_bytes());
    }
}

// redis_ping sends the one RESP line the demo needs and returns the "+PONG"
// reply — enough to prove "redis" resolved over the shared network, no crate.
fn redis_ping(addr: &str) -> std::io::Result<String> {
    let sockaddr = addr
        .to_socket_addrs()?
        .next()
        .ok_or_else(|| std::io::Error::new(std::io::ErrorKind::NotFound, "name did not resolve"))?;
    let mut stream = TcpStream::connect_timeout(&sockaddr, Duration::from_secs(2))?;
    stream.set_read_timeout(Some(Duration::from_secs(2)))?;
    stream.set_write_timeout(Some(Duration::from_secs(2)))?;
    stream.write_all(b"PING\r\n")?;
    let mut buf = [0u8; 64];
    let n = stream.read(&mut buf)?;
    Ok(String::from_utf8_lossy(&buf[..n]).trim().to_string())
}
