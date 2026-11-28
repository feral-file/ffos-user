#include "ff/artwork_fade.hpp"

#include <cmath>
#include <iostream>

int main() {
  const auto cross = ff::artwork_fade(325, 650, false);
  const auto out = ff::artwork_fade(325, 650, true);
  const auto midpoint = ff::artwork_fade(650, 650, true);
  const auto in = ff::artwork_fade(975, 650, true);
  const auto end = ff::artwork_fade(1300, 650, true);
  const auto cut = ff::artwork_fade(0, 0, true);
  if (std::abs(cross.incoming - .8024f) > .001 || cross.complete ||
      cross.outgoing + cross.incoming != 1 ||
      out.incoming != 0 || out.outgoing != cross.outgoing || out.complete ||
      midpoint.outgoing != 0 || midpoint.incoming != 0 || midpoint.complete ||
      in.outgoing != 0 || in.incoming != cross.incoming || in.complete ||
      !end.complete || end.incoming != 1 || !cut.complete || cut.incoming != 1) {
    std::cerr << "Legacy fade opacity or phase boundary changed\n";
    return 1;
  }
}
