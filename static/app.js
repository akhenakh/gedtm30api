document.addEventListener("DOMContentLoaded", () => {
  let points = []; // Array of [lat, lng]
  let markers = []; // Array of Leaflet markers
  let polyline = null;
  let profileChart = null;

  const infoDiv = document.getElementById("info");
  const resetButton = document.getElementById("resetButton");
  const chartCanvas = document.getElementById("profileChart");

  const map = L.map("map").setView([45.832119, 6.865575], 14);
  L.tileLayer("https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png", {
    maxZoom: 19,
    attribution: "© OpenStreetMap contributors",
  }).addTo(map);

  map.on("click", onMapClick);
  resetButton.addEventListener("click", resetAll);

  /**
   * Handles clicks on the map.
   */
  async function onMapClick(e) {
    const { lat, lng } = e.latlng;

    points.push([lat, lng]);
    const marker = L.marker([lat, lng]).addTo(map);
    markers.push(marker);

    if (points.length === 1) {
      infoDiv.textContent = "Fetching elevation...";
      // This call is not awaited, which is fine for a single point.
      getSinglePointElevation(lat, lng);
    } else {
      infoDiv.textContent = "Fetching elevation profile...";
      updatePolyline();
      // We use `await` to ensure we wait for the profile data before proceeding.
      await getProfile();
    }
  }

  /**
   * Fetches elevation for a single point.
   */
  async function getSinglePointElevation(lat, lng) {
    try {
      const response = await fetch(`/getElevation/${lat}/${lng}`);
      if (!response.ok) {
        throw new Error(`Server error: ${response.statusText}`);
      }
      const data = await response.json();
      infoDiv.innerHTML = `Elevation: <strong>${data.elevation.toFixed(1)} m</strong>. Click another point to create a profile.`;
    } catch (error) {
      infoDiv.textContent = `Error: ${error.message}`;
      console.error("Failed to fetch elevation:", error);
    }
  }

  /**
   * Fetches the elevation profile for the current list of points.
   */
  async function getProfile() {
    if (points.length < 2) return;

    try {
      const response = await fetch("/getProfile/", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(points),
      });

      if (!response.ok) {
        const errorText = await response.text();
        throw new Error(`Failed to get profile: ${errorText}`);
      }

      const profileData = await response.json();

      if (profileData && profileData.length > 0) {
        infoDiv.textContent = `Profile generated for ${points.length} points.`;
        renderChart(profileData);
      } else {
        infoDiv.textContent = "Received empty profile from server.";
        console.warn("Received empty or invalid profile data:", profileData);
      }
    } catch (error) {
      infoDiv.textContent = `Error fetching profile: ${error.message}`;
      console.error("Failed to fetch or process profile:", error);
    }
  }

  /**
   * Draws or updates the polyline connecting markers on the map.
   */
  function updatePolyline() {
    const latLngs = points.map((p) => [p[0], p[1]]);
    if (!polyline) {
      polyline = L.polyline(latLngs, { color: "red" }).addTo(map);
    } else {
      polyline.setLatLngs(latLngs);
    }
  }

  /**
   * Renders or updates the elevation profile chart.
   * @param {Array<Array<number>>} profileData - Array of [lat, lng, elv] arrays.
   */
  function renderChart(profileData) {
    if (profileChart) {
      profileChart.destroy();
    }

    if (!profileData || profileData.length === 0) {
      console.error("renderChart called with no data.");
      return;
    }

    let cumulativeDistance = 0;
    const distances = [0];
    for (let i = 1; i < profileData.length; i++) {
      const p1 = profileData[i - 1]; // p1 is [lat, lng, elv]
      const p2 = profileData[i]; // p2 is [lat, lng, elv]
      cumulativeDistance += haversineDistance(p1[0], p1[1], p2[0], p2[1]);
      distances.push(cumulativeDistance);
    }

    const labels = distances.map((d) => d.toFixed(2));
    const elevations = profileData.map((p) => p[2]);

    const ctx = chartCanvas.getContext("2d");
    profileChart = new Chart(ctx, {
      type: "line",
      data: {
        labels: labels,
        datasets: [
          {
            label: "Elevation Profile",
            data: elevations,
            borderColor: "rgb(75, 192, 192)",
            backgroundColor: "rgba(75, 192, 192, 0.2)",
            fill: true,
            tension: 0.1,
            pointRadius: 0,
          },
        ],
      },
      // The options block remains exactly the same.
      options: {
        responsive: true,
        maintainAspectRatio: false,
        plugins: {
          title: { display: true, text: "Elevation Profile" },
          tooltip: {
            callbacks: {
              label: function (context) {
                let label = context.dataset.label || "";
                if (label) {
                  label += ": ";
                }
                if (context.parsed.y !== null) {
                  label += `${context.parsed.y.toFixed(1)} m`;
                }
                return label;
              },
            },
          },
        },
        scales: {
          x: { title: { display: true, text: "Distance (km)" } },
          y: { title: { display: true, text: "Elevation (m)" } },
        },
      },
    });
  }

  /**
   * Resets the map, chart, and application state.
   */
  function resetAll() {
    points = [];
    markers.forEach((marker) => map.removeLayer(marker));
    markers = [];
    if (polyline) {
      map.removeLayer(polyline);
      polyline = null;
    }
    if (profileChart) {
      profileChart.destroy();
      profileChart = null;
    }
    infoDiv.textContent = "Click on the map to add points.";
  }

  /**
   * Calculates the Haversine distance between two points.
   * @returns {number} Distance in kilometers.
   */
  function haversineDistance(lat1, lon1, lat2, lon2) {
    const R = 6371; // Radius of the Earth in km
    const dLat = ((lat2 - lat1) * Math.PI) / 180;
    const dLon = ((lon2 - lon1) * Math.PI) / 180;
    const a =
      Math.sin(dLat / 2) * Math.sin(dLat / 2) +
      Math.cos((lat1 * Math.PI) / 180) *
        Math.cos((lat2 * Math.PI) / 180) *
        Math.sin(dLon / 2) *
        Math.sin(dLon / 2);
    const c = 2 * Math.atan2(Math.sqrt(a), Math.sqrt(1 - a));
    return R * c;
  }
});
