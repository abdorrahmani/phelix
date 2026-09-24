// compose-rust-demo: a minimal Rust HTTP service that demonstrates the Phelix
// Docker runtime with SPLIT OWNERSHIP. Phelix builds an image for this app and
// runs each instance as a container attached to a user-defined Docker network;
// a backing service (Redis) is owned separately by docker-compose on that same
// network. The app reaches Redis by its compose DNS name (redis:6379) — proof
// that deploy.network wired the two tiers together. Standard library only: a
// raw RESP PING, no Redis crate.
use std::env;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream, ToSocketAddrs};
use std::process;
use std::time::Duration;

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

    // Addressed by docker-compose SERVICE NAME, which resolves only because
    // Phelix attached this container to the compose network (deploy.network). In
    // production this comes from encrypted env
    // (`phelix env set compose-rust-demo REDIS_ADDR redis:6379`); the default
    // matches the compose service so the example runs with no env setup.
    let redis_addr = env::var("REDIS_ADDR").unwrap_or_else(|_| "redis:6379".to_string());

    let addr = format!("0.0.0.0:{port_num}");
    let listener = match TcpListener::bind(&addr) {
        Ok(l) => l,
        Err(e) => {
            eprintln!("failed to bind {addr}: {e}");
            process::exit(1);
        }
    };
    println!("compose-rust-demo listening on {addr} (redis at {redis_addr})");

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

        let (status, body) = match path.as_str() {
            // Liveness ONLY — deliberately independent of Redis so a backing
            // blip never fails a blue-green/rolling health gate.
            "/health" => ("200 OK", "ok\n".to_string()),
            // Proves the shared-network wiring: dial Redis by DNS name and report
            // what it answered.
            "/redis" => match ping_redis(&redis_addr) {
                Ok(pong) => ("200 OK", format!("redis {redis_addr} -> {pong}\n")),
                Err(e) => (
                    "503 Service Unavailable",
                    format!("redis {redis_addr} unreachable: {e}\n"),
                ),
            },
            _ => (
                "200 OK",
                format!("hello from compose-rust-demo on port {port_num}\n"),
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

// ping_redis speaks the one line of the Redis protocol this demo needs: send
// PING, read the "+PONG" reply. Raw TCP keeps the app crate-free — the point is
// that "redis" resolves over the shared Docker network, not the client library.
fn ping_redis(addr: &str) -> std::io::Result<String> {
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
