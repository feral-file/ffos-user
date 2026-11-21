find_program(GO_EXECUTABLE go REQUIRED)
set(EGRESS_DIR "${CMAKE_CURRENT_SOURCE_DIR}/egress")
add_custom_command(OUTPUT "${CMAKE_CURRENT_BINARY_DIR}/libff_network.so"
  COMMAND "${GO_EXECUTABLE}" build -buildmode=c-shared -ldflags=-extldflags=-Wl,-soname,libff_network.so -o "${CMAKE_CURRENT_BINARY_DIR}/libff_network.so" .
  WORKING_DIRECTORY "${EGRESS_DIR}" DEPENDS egress/main.go egress/http.go egress/routing.go ../feral-controld/contentpath/path.go ../feral-controld/contentpath/cid.go egress/entry.go egress/go.mod)
add_custom_target(artwork-egress ALL DEPENDS "${CMAKE_CURRENT_BINARY_DIR}/libff_network.so")
add_test(NAME artwork-egress COMMAND "${GO_EXECUTABLE}" test ./...)
set_tests_properties(artwork-egress PROPERTIES WORKING_DIRECTORY "${EGRESS_DIR}")
add_test(NAME artwork-network-namespace COMMAND "${Python3_EXECUTABLE}"
  "${CMAKE_CURRENT_SOURCE_DIR}/tests/egress_sandbox_test.py" "$<TARGET_FILE:ff-player-native>")
set_tests_properties(artwork-network-namespace PROPERTIES SKIP_RETURN_CODE 77 TIMEOUT 25)
add_test(NAME artwork-public-route COMMAND bash
  "${CMAKE_CURRENT_SOURCE_DIR}/tests/egress_public_route_test.sh" "$<TARGET_FILE:ff-player-native>")
set_tests_properties(artwork-public-route PROPERTIES SKIP_RETURN_CODE 77 TIMEOUT 35)
install(FILES "${CMAKE_CURRENT_BINARY_DIR}/libff_network.so" DESTINATION lib/ff-player-native)
find_package(PkgConfig REQUIRED)
pkg_check_modules(CURL REQUIRED IMPORTED_TARGET libcurl)
add_library(ff_cache_client src/cache_client.cpp src/profile_gc.cpp)
pkg_check_modules(GLIB REQUIRED IMPORTED_TARGET glib-2.0)
target_link_libraries(ff_cache_client PUBLIC ff_core PkgConfig::GLIB)
add_executable(bundle-reader-tests tests/bundle_reader_test.cpp)
target_link_libraries(bundle-reader-tests PRIVATE ff_cache_client)
add_test(NAME bundle-reader COMMAND bundle-reader-tests)
pkg_check_modules(NATIVE REQUIRED IMPORTED_TARGET sdl2 egl glesv2 libdrm pangocairo pangoft2 fontconfig libqrencode wayland-client wayland-egl)
add_library(ff_artwork_cache src/artwork_cache.cpp)
target_link_libraries(ff_artwork_cache PUBLIC ff_core PkgConfig::CURL PkgConfig::NATIVE)
pkg_check_modules(MPV REQUIRED IMPORTED_TARGET mpv>=2.6.0)
add_library(ff_decode SHARED src/media_worker.cpp)
target_link_libraries(ff_decode PRIVATE ff_media_ipc PkgConfig::MPV PkgConfig::NATIVE)
add_executable(media-decode-tests tests/media_decode_test.cpp)
target_link_libraries(media-decode-tests PRIVATE PkgConfig::MPV)
add_test(NAME media-decode COMMAND media-decode-tests ${CMAKE_CURRENT_SOURCE_DIR}/tests/media-h264-aac.mp4)
set_tests_properties(media-decode PROPERTIES TIMEOUT 25)
set_target_properties(ff_decode PROPERTIES INSTALL_RPATH "$ORIGIN")
install(TARGETS ff_decode LIBRARY DESTINATION lib/ff-player-native/media)
set(FF_MEDIA_PREFIX "${CMAKE_CURRENT_SOURCE_DIR}/build-media/prefix" CACHE PATH "Pinned private media libraries")
execute_process(COMMAND ${Python3_EXECUTABLE} ${CMAKE_CURRENT_SOURCE_DIR}/tools/prepare-renderers.py
  COMMAND_ERROR_IS_FATAL ANY)
if(NOT EXISTS "${FF_MEDIA_PREFIX}/lib/libmpv.so")
  message(FATAL_ERROR "Build the pinned private media dependencies with tools/build-ffmpeg.sh and tools/build-media.sh")
endif()
install(DIRECTORY "${FF_MEDIA_PREFIX}/lib/" DESTINATION lib/ff-player-native/media
        FILES_MATCHING PATTERN "*.so*" PATTERN pkgconfig EXCLUDE)
install(FILES "${CMAKE_CURRENT_SOURCE_DIR}/vendor/mpv-0.42.0/LICENSE.LGPL"
        DESTINATION share/licenses/ff-player-native RENAME mpv-LGPL.txt)
install(FILES "${CMAKE_CURRENT_SOURCE_DIR}/vendor/mpv-0.42.0/Copyright"
        DESTINATION share/licenses/ff-player-native RENAME mpv-Copyright.txt)
install(FILES "${CMAKE_CURRENT_SOURCE_DIR}/vendor/libplacebo-7.387.0/LICENSE"
        DESTINATION share/licenses/ff-player-native RENAME libplacebo.txt)
install(FILES "${CMAKE_CURRENT_SOURCE_DIR}/vendor/ffmpeg/COPYING.LGPLv2.1"
        DESTINATION share/licenses/ff-player-native RENAME FFmpeg-LGPL.txt)
pkg_get_variable(WAYLAND_PROTOCOLS_DIR wayland-protocols pkgdatadir)
find_program(WAYLAND_SCANNER wayland-scanner REQUIRED)
set(PRESENTATION_XML "${WAYLAND_PROTOCOLS_DIR}/stable/presentation-time/presentation-time.xml")
install(FILES "${PRESENTATION_XML}" DESTINATION share/licenses/ff-player-native RENAME wayland-presentation-time.xml)
add_custom_command(OUTPUT presentation-time-client-protocol.h presentation-time-protocol.c
  COMMAND ${WAYLAND_SCANNER} client-header ${PRESENTATION_XML} presentation-time-client-protocol.h
  COMMAND ${WAYLAND_SCANNER} private-code ${PRESENTATION_XML} presentation-time-protocol.c
  DEPENDS ${PRESENTATION_XML} VERBATIM)
set(XDG_SHELL_XML "${WAYLAND_PROTOCOLS_DIR}/stable/xdg-shell/xdg-shell.xml")
add_custom_command(OUTPUT xdg-shell-client-protocol.h xdg-shell-protocol.c
  COMMAND ${WAYLAND_SCANNER} client-header ${XDG_SHELL_XML} xdg-shell-client-protocol.h
  COMMAND ${WAYLAND_SCANNER} private-code ${XDG_SHELL_XML} xdg-shell-protocol.c
  DEPENDS ${XDG_SHELL_XML} VERBATIM)
add_library(ff_presentation_feedback STATIC xdg-shell-client-protocol.h xdg-shell-protocol.c src/cage_overlay.cpp presentation-time-protocol.c src/presentation_feedback.cpp)
target_include_directories(ff_presentation_feedback PRIVATE "${CMAKE_CURRENT_BINARY_DIR}")
target_link_libraries(ff_presentation_feedback PRIVATE ff_core PkgConfig::NATIVE)
target_link_libraries(ff_decode PRIVATE ff_presentation_feedback)
add_library(ff_device_ui src/device_ui.cpp)
target_link_libraries(ff_device_ui PUBLIC ff_core PkgConfig::NATIVE)
add_executable(device-ui-tests tests/device_ui_test.cpp)
target_link_libraries(device-ui-tests PRIVATE ff_device_ui)
add_test(NAME device-ui COMMAND device-ui-tests ${CMAKE_CURRENT_SOURCE_DIR}/assets/fonts ${CMAKE_CURRENT_BINARY_DIR}/ui-evidence)
