#include "ff/cache_client.hpp"

#include <poll.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

#include <chrono>
#include <cstring>
#include <fstream>
#include <iostream>
#include <thread>
int main(int argc, char** argv) {
  try {
    ff::require(argc == 2, "test", "fixture required");
    std::ifstream file(argv[1]);
    auto fixture = ff::Json::parse(file);
    auto path = "/tmp/ff-cache-test-" + ff::uuid();
    int listener = socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
    sockaddr_un address{};
    address.sun_family = AF_UNIX;
    std::strcpy(address.sun_path, path.c_str());
    ff::require(bind(listener, reinterpret_cast<sockaddr*>(&address), sizeof(address)) == 0 &&
                    listen(listener, 8) == 0,
                "test", "socket setup");
    auto metadata = fixture.at("policyResponse").dump();
    std::string body(4, '\0');
    auto length = uint32_t(metadata.size());
    for (int i = 0; i < 4; i++) body[i] = char(length >> (24 - i * 8));
    body += metadata;
    auto response =
        "HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: " + std::to_string(body.size()) +
        "\r\n\r\n" + body;
    std::jthread server([&](std::stop_token stop) {
      for (int i = 0; i < 65; i++) {
        pollfd ready{listener, POLLIN, 0};
        while (!stop.stop_requested() && poll(&ready, 1, 100) <= 0) {
        }
        if (stop.stop_requested()) return;
        int fd = accept4(listener, nullptr, nullptr, SOCK_CLOEXEC);
        if (fd < 0) return;
        char request[8192];
        recv(fd, request, sizeof(request), 0);
        size_t offset = 0;
        while (offset < response.size()) {
          auto n = send(fd, response.data() + offset, response.size() - offset, MSG_NOSIGNAL);
          if (n <= 0) break;
          offset += n;
        }
        close(fd);
      }
    });
    auto inbox = std::make_shared<ff::CacheInbox>();
    for (int i = 0; i < 64; i++)
      ff::require(
          ff::cache_request(inbox, path, "", std::to_string(i), fixture.at("request"), false, true),
          "test", "accepted burst unexpectedly rejected");
    ff::require(!ff::cache_request(inbox, path, "", "overflow", fixture.at("request"), false, true),
                "test", "inbox admission bound");
    auto until = std::chrono::steady_clock::now() + std::chrono::seconds(10);
    size_t count = 0;
    while (std::chrono::steady_clock::now() < until) {
      {
        std::lock_guard lock(inbox->mutex);
        count = inbox->replies.size();
      }
      if (count == 64) break;
      std::this_thread::sleep_for(std::chrono::milliseconds(10));
    }
    auto replies = inbox->take();
    ff::require(replies.size() == 64, "test", "accepted completion was dropped");
    for (const auto& r : replies)
      ff::require(r.error.empty() && r.metadata == fixture.at("policyResponse"), "test", r.error);
    ff::require(
        ff::cache_request(inbox, path, "", "after-drain", fixture.at("request"), false, true),
        "test", "drain did not release capacity");
    server.join();
    close(listener);
    unlink(path.c_str());
    std::cout << "64 accepted policy requests completed without draining; capacity released; "
                 "shared metadata verified\n";
  } catch (const std::exception& e) {
    std::cerr << e.what() << '\n';
    return 1;
  }
}
