// aware-rust-demo demonstrates PATTERN B in Rust: docker-compose is the source
// of truth for the app's BUILD (context/args/target), and Phelix builds the
// image FROM that compose service (deploy.docker.build: compose) rather than
// from a standalone Dockerfile it owns. Runtime env still comes from
// `phelix env`, not the compose service's environment. Standard library only.
use std::env;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream, ToSocketAddrs};
use std::process;
use std::time::Duration;

fn main() {
    let port = env::var("PORT").unwrap_or_else(|_| "8080".to_string());
    let port_num: u16 = match port.parse() {
        Ok(n) if n >= 1 => n,
        _ => {
            eprintln!("invalid PORT {port:?}: must be an integer in 1-65535");
            process::exit(1);
        }
    };
    // Backing services by docker-compose service name over the shared network.
    // In production these come from encrypted env, NOT the compose file:
    //   phelix env set aware-rust-demo DATABASE_URL mysql://user:pass@mysql:3306/app
    //   phelix env set aware-rust-demo REDIS_ADDR   redis:6379
    let mysql_addr = env::var("MYSQL_ADDR").unwrap_or_else(|_| "mysql:3306".to_string());
    let redis_addr = env::var("REDIS_ADDR").unwrap_or_else(|_| "redis:6379".to_string());

    let addr = format!("0.0.0.0:{port_num}");
    let listener = match TcpListener::bind(&addr) {
        Ok(l) => l,
        Err(e) => {
            eprintln!("failed to bind {addr}: {e}");
            process::exit(1);
        }
    };
    println!("aware-rust-demo listening on {addr} (mysql={mysql_addr} redis={redis_addr})");

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
            "/health" => ("200 OK", "ok\n".to_string()), // liveness only
            "/backends" => {
                let mysql_line = format!("mysql {mysql_addr} -> {}\n", reachable(&mysql_addr));
                let redis_line = match redis_ping(&redis_addr) {
                    Ok(pong) => format!("redis {redis_addr} -> {pong}\n"),
                    Err(e) => format!("redis {redis_addr} -> unreachable: {e}\n"),
                };
                ("200 OK", mysql_line + &redis_line)
            }
            _ => (
                "200 OK",
                format!("hello from aware-rust-demo on port {port_num}\n"),
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

// reachable opens a TCP connection to addr and reports whether it succeeded —
// enough to prove the compose service name resolved over the shared network
// (no MySQL driver needed for the demo).
fn reachable(addr: &str) -> String {
    let sockaddr = match addr.to_socket_addrs().ok().and_then(|mut it| it.next()) {
        Some(sa) => sa,
        None => return "unreachable: name did not resolve".to_string(),
    };
    match TcpStream::connect_timeout(&sockaddr, Duration::from_secs(2)) {
        Ok(_) => "reachable".to_string(),
        Err(e) => format!("unreachable: {e}"),
    }
}

// redis_ping sends the one RESP line the demo needs and returns the "+PONG" reply.
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
