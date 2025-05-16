#!/bin/bash

# Exit immediately if a command exists with a non-zero status
set -e

echo "Building gophel..."

go build -o gophel

echo "Moving gophel to /usr/local/bin/..."
sudo mv gophel /usr/local/bin/

echo "Setting executable permission"
sudo chmod +x /usr/local/bin/gophel

# Create startup script
echo "Creating startup script..."
sudo tee /usr/local/bin/gophel-startup.sh > /dev/null << EOL
#!/bin/bash
# Start the monitoring service
/usr/local/bin/gophel monitor &

# Wait for monitoring service to initialize
sleep 5

# Start all applications
/usr/local/bin/gophel start
EOL

# Make startup script executable
sudo chmod +x /usr/local/bin/gophel-startup.sh

# Create systemd service file
echo "Creating systemd service..."
sudo tee /etc/systemd/system/gophel.service > /dev/null << EOL
[Unit]
Description=Gophel WebSocket Monitoring Service
After=network.target

[Service]
Type=simple
User=$USER
WorkingDirectory=/usr/local/bin
ExecStart=/usr/local/bin/gophel-startup.sh
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOL

# Reload systemd to recognize new service
echo "Reloading systemd..."
sudo systemctl daemon-reload

# Enable and start the service
echo "Enabling and starting gophel service..."
sudo systemctl enable gophel.service
sudo systemctl restart gophel.service

echo "✅ Installation completed! You can now run 'gophel --help'."
echo "The WebSocket monitoring service is running in the background."
echo "To check service status: sudo systemctl status gophel"
echo "To view logs: sudo journalctl -u gophel -f"