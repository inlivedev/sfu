export class InliveVideoObserver {
    #lastReportTime

    #intervalGap

    #dataChannel

    #videoElements

    #resizeObserver

    #intersectionObserver
    /**
     * Constructor.
     * @param {RTCDataChannel} dataChannel - Data channel to use for reporting video size
     * @param {number} intervalGap - interval time gap between report
     * @returns {void}
     */
    constructor(dataChannel,intervalGap) {
        if (typeof intervalGap !== 'number') {
            this.#intervalGap = 1000
        } else {
            this.#intervalGap = intervalGap
        }
       
        this.#dataChannel = dataChannel
        this.#videoElements = []
        this.#resizeObserver = new ResizeObserver(this.#onResize.bind(this))
        this.#intersectionObserver = new IntersectionObserver(this.#onIntersection.bind(this))
    }

    /**
     * Callback when video element is resized.
     * @param {ResizeObserverEntry[]} entries - Resize observer entries
     * @returns {void}
     */
    #onResize(entries) {
        entries.forEach(entry => {
            if (entry.contentBoxSize) {
                const videoTracks=entry.target.srcObject.getVideoTracks();
                if (videoTracks.length > 0) {
                    const trackid=videoTracks[0].id;
                    const contentBoxSize = entry.contentBoxSize[0];
                    const width = contentBoxSize.inlineSize;
                    const height = contentBoxSize.blockSize;
                    this.#onVideoSizeChanged(trackid,width, height);
                }
            }
        });
    }

    /**
     * Callback when video element is intersected.
     * @param {IntersectionObserverEntry[]} entries - Intersection observer entries
     * @returns {void}
     */
    #onIntersection(entries) {
        entries.forEach(entry => {
                const videoTracks=entry.target.srcObject.getVideoTracks();
                if (videoTracks.length > 0) {
                    const trackid=videoTracks[0].id;
                    const width = entry.isIntersecting?entry.target.width:0;
                    const height = entry.isIntersecting?entry.target.height:0;
                    this.#onVideoSizeChanged(trackid,width, height);
                }
        });
    }


    /**
     * Observe video element for any visibility or resize changes.
     * @param {HTMLVideoElement} videoElement - Video element to watch
     * @returns {void}
     */
    observe(videoElement) {
        this.#watchVideoElement(videoElement)
        this.#videoElements.push(videoElement)
    }

    /**
     * Watch video element events.
     * @param {HTMLVideoElement} videoElement - Video element to watch
     * @returns {void}
     */
    #watchVideoElement(videoElement) {
        this.#intersectionObserver.observe(videoElement)
        this.#resizeObserver.observe(videoElement)
    }

    /**
     * Report video size to peer connection.
     * @param {string} id - MediaStreamTrack id
     * @param {number} width - Video width
     * @param {number} height - Video height
     * @returns {void}
     */
    #onVideoSizeChanged(id,width, height) {
        if (this.#lastReportTime !== null && (Date.now() - this.#lastReportTime) < this.#intervalGap) {
            return;
        }

        this.#lastReportTime = Date.now();

        if(this.#dataChannel.readyState == "open"){
            this.#dataChannel.send(JSON.stringify({
                type: 'video_size',
                data: {
                    track_id: id,
                    width: Math.floor(width),
                    height:  Math.floor(height)
                }
            }));
        } else {
            const listener = () => {
                this.#dataChannel.send(JSON.stringify({
                    type: 'video_size',
                    data: {
                        track_id: id,
                        width:  Math.floor(width),
                        height:  Math.floor(height)
                    }
                }));

                this.#dataChannel.removeEventListener('open', listener);
            };
            
            this.#dataChannel.addEventListener('open', listener);
        }
        
    }
}